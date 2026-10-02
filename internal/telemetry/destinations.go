package telemetry

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/goobers/goobers/internal/journal"
)

// NamedDestination is a remote transport with a stable operator-selected name.
// Config contains only remote settings; local exporters and nested destinations
// are rejected to prevent duplicate local journal writes.
type NamedDestination struct {
	Name   string
	Config Config
}

var destinationNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func validateNamedConfig(cfg Config) error {
	destinations := cfg.Destinations
	if len(destinations) > 0 && (cfg.Exporter != "" || cfg.OTLPEndpoint != "" || cfg.AzureMonitorConnectionString != "") {
		return errors.New("named telemetry destinations cannot be combined with legacy remote settings")
	}
	if len(destinations) > 16 {
		return errors.New("at most 16 telemetry destinations are supported")
	}
	names := make(map[string]bool)
	spools := make(map[string]bool)
	for _, d := range destinations {
		if !destinationNamePattern.MatchString(d.Name) || names[d.Name] {
			return errors.New("telemetry destination names must be unique and match [a-z][a-z0-9-]{0,63}")
		}
		names[d.Name] = true
		c := d.Config
		if c.AzureMonitorReplayRoot != "" {
			root, err := canonicalReplayRoot(c.AzureMonitorReplayRoot)
			if err != nil {
				return fmt.Errorf("destination %s: invalid replay root", d.Name)
			}
			if runtime.GOOS == "windows" {
				root = strings.ToLower(root)
			}
			if spools[root] {
				return errors.New("named telemetry destinations must use independent replay roots")
			}
			spools[root] = true
		}
		if len(c.Destinations) != 0 || c.SpanExporter != nil || c.MetricReader != nil || c.MetricExporter != nil || c.Stdout != nil {
			return fmt.Errorf("destination %s must contain only remote transport settings", d.Name)
		}
		if !validNamedTransport(c) {
			return fmt.Errorf("destination %s requires exactly one OTLP/gRPC or Azure Monitor transport", d.Name)
		}
	}
	return nil
}

func prepareNamedDestinations(cfg Config) Config {
	destinations := append([]NamedDestination(nil), cfg.Destinations...)
	for i := range destinations {
		c := &destinations[i].Config
		c.Scrubber, c.JournalInstanceID = cfg.Scrubber, cfg.JournalInstanceID
		c.AzureMonitorReplayStart = cfg.AzureMonitorReplayStart
		c.MetricExportInterval = cfg.MetricExportInterval
		if cfg.ExporterHealth != nil {
			mode := string(c.Exporter)
			if c.AzureMonitorConnectionString != "" {
				mode = "azure-monitor"
			}
			c.ExporterHealth = cfg.ExporterHealth.destination(destinations[i].Name, mode, c.OTLPEndpoint)
		}
	}
	cfg.Destinations = destinations
	return cfg
}

func namedSpanProcessors(ctx context.Context, cfg Config) (parallelSpanProcessors, error) {
	var processors parallelSpanProcessors
	var errs []error
	for _, destination := range cfg.Destinations {
		exporters, err := spanExporters(ctx, destination.Config)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: destination %s: %w", ErrOTLPUnavailable, destination.Name, err))
			if destination.Config.AzureMonitorTraces {
				destination.Config.ExporterHealth.RecordTraceFailure(err)
			}
		}
		for _, exporter := range exporters {
			// Each remote owns a bounded nonblocking queue, even when local spans
			// use synchronous export. A blocked destination cannot block OnEnd.
			processors = append(processors, sdktrace.NewBatchSpanProcessor(exporter))
		}
	}
	return processors, errors.Join(errs...)
}

type parallelSpanProcessors []sdktrace.SpanProcessor

func (p parallelSpanProcessors) OnStart(ctx context.Context, span sdktrace.ReadWriteSpan) {
	for _, processor := range p {
		processor.OnStart(ctx, span)
	}
}
func (p parallelSpanProcessors) OnEnd(span sdktrace.ReadOnlySpan) {
	for _, processor := range p {
		processor.OnEnd(span)
	}
}
func (p parallelSpanProcessors) Shutdown(ctx context.Context) error {
	return parallelDestinationCalls(len(p), func(i int) error { return p[i].Shutdown(ctx) })
}
func (p parallelSpanProcessors) ForceFlush(ctx context.Context) error {
	return parallelDestinationCalls(len(p), func(i int) error { return p[i].ForceFlush(ctx) })
}

// Every destination receives the same deadline concurrently. A failed endpoint
// cannot consume the healthy endpoint's entire flush or shutdown budget.
func parallelDestinationCalls(count int, call func(int) error) error {
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = call(i) }()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (c *Client) configureAllJournalLogs(ctx context.Context, cfg Config, res *resource.Resource) error {
	if len(cfg.Destinations) == 0 {
		return c.configureJournalLogs(ctx, cfg, res)
	}
	var errs []error
	for _, destination := range cfg.Destinations {
		child := &Client{scrubber: c.scrubber, destinationName: destination.Name}
		childCfg := destination.Config
		childCfg.JournalRoot = "" // The parent alone owns the root registration.
		if err := child.configureJournalLogs(ctx, childCfg, res); err != nil {
			errs = append(errs, fmt.Errorf("%w: destination %s: %w", ErrOTLPUnavailable, destination.Name, err))
		}
		if child.journalLogs == nil {
			continue
		}
		child.journalLogs.observeDrops = c.journalExportDropped
		if cfg.JournalRoot != "" && childCfg.AzureMonitorReplayRoot != "" {
			childCfg.JournalRoot = cfg.JournalRoot
			child.journalCatchup = newJournalCatchup(childCfg, child.journalLogs)
		}
		c.journalDestinations = append(c.journalDestinations, child)
	}
	if len(c.journalDestinations) > 0 && cfg.JournalRoot != "" {
		unregister, err := journal.RegisterCommittedEventSink(cfg.JournalRoot, cfg.JournalInstanceID, c)
		if err != nil {
			c.shutdownJournalDestinations(ctx)
			return errors.Join(append(errs, fmt.Errorf("%w: register journal logs: %w", ErrOTLPUnavailable, err))...)
		}
		c.unregisterJournal = unregister
	}
	return errors.Join(errs...)
}

func (c *Client) flushJournalDestinations(ctx context.Context) {
	_ = parallelDestinationCalls(len(c.journalDestinations), func(i int) error { return c.journalDestinations[i].Flush(ctx) })
}
func (c *Client) shutdownJournalDestinations(ctx context.Context) {
	_ = parallelDestinationCalls(len(c.journalDestinations), func(i int) error { return c.journalDestinations[i].Shutdown(ctx) })
}

func (c *Client) startJournalDestinationCalls(ctx context.Context, shutdown bool) func() {
	if len(c.journalDestinations) == 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if shutdown {
			c.shutdownJournalDestinations(ctx)
		} else {
			c.flushJournalDestinations(ctx)
		}
	}()
	return func() { <-done }
}

// DestinationJournalStats reports counters separately, since delivery/replay
// acknowledgements and cursor progress are independent for each destination.
func (c *Client) DestinationJournalStats() map[string]JournalExportStats {
	if c == nil {
		return nil
	}
	stats := make(map[string]JournalExportStats, len(c.journalDestinations))
	for _, child := range c.journalDestinations {
		stats[child.destinationName] = child.JournalExportStats()
	}
	return stats
}

// Every constructor must select the same single transport. In particular the
// diagnostic constructor consumes OTLPEndpoint directly, independent of Exporter.
func validNamedTransport(c Config) bool {
	if c.AzureMonitorConnectionString != "" {
		return c.Exporter == "" && c.OTLPEndpoint == "" && !c.OTLPInsecure && len(c.OTLPHeaders) == 0 && c.OTLPCAFile == "" && c.OTLPServerName == "" && c.OTLPCertFile == "" && c.OTLPKeyFile == ""
	}
	return c.Exporter == ExporterOTLP && c.OTLPEndpoint != ""
}
