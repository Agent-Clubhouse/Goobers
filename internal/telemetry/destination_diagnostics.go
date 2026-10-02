package telemetry

import (
	"context"
	"errors"
	"fmt"
)

func newNamedDiagnosticExporter(cfg Config) (*DiagnosticExporter, error) {
	if err := validateNamedConfig(cfg); err != nil {
		return nil, err
	}
	parent := &DiagnosticExporter{destinations: make(map[string]*DiagnosticExporter)}
	var errs []error
	for _, destination := range cfg.Destinations {
		childCfg := destination.Config
		childCfg.Scrubber = cfg.Scrubber
		childCfg.ServiceVersion, childCfg.BuildCommit = cfg.ServiceVersion, cfg.BuildCommit
		childCfg.ResourceAttributes = append(cfg.ResourceAttributes[:len(cfg.ResourceAttributes):len(cfg.ResourceAttributes)], childCfg.ResourceAttributes...)
		childCfg.AzureMonitorReplayStart = cfg.AzureMonitorReplayStart
		child, err := NewDiagnosticExporter(childCfg)
		if err != nil {
			errs = append(errs, fmt.Errorf("destination %s: %w", destination.Name, err))
		}
		if child != nil {
			parent.destinations[destination.Name] = child
		}
	}
	if len(parent.destinations) == 0 {
		return nil, errors.Join(errs...)
	}
	return parent, errors.Join(errs...)
}

// DestinationStats reports independent diagnostic queue and replay counters.
func (d *DiagnosticExporter) DestinationStats() map[string]DiagnosticExportStats {
	if d == nil || len(d.destinations) == 0 {
		return nil
	}
	stats := make(map[string]DiagnosticExportStats, len(d.destinations))
	for name, child := range d.destinations {
		stats[name] = child.Stats()
	}
	return stats
}

func (d *DiagnosticExporter) namedDiagnosticStats() DiagnosticExportStats {
	var total DiagnosticExportStats
	for _, s := range d.DestinationStats() {
		total.Accepted += s.Accepted
		total.Delivered += s.Delivered
		total.Dropped += s.Dropped
		total.Failures += s.Failures
	}
	return total
}

func (d *DiagnosticExporter) shutdownNamedDiagnostics(ctx context.Context) error {
	children := make([]*DiagnosticExporter, 0, len(d.destinations))
	for _, child := range d.destinations {
		children = append(children, child)
	}
	return parallelDestinationCalls(len(children), func(i int) error { return children[i].Shutdown(ctx) })
}
