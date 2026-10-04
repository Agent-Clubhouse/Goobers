package telemetry

import (
	"context"
	"errors"
	"fmt"
)

func newNamedDiagnosticExporter(cfg Config) (*DiagnosticExporter, error) {
	validationCfg := cfg
	validationCfg.Exporter, validationCfg.OTLPEndpoint, validationCfg.AzureMonitorConnectionString = "", "", ""
	if err := validateNamedConfig(validationCfg); err != nil {
		return nil, err
	}
	parent := &DiagnosticExporter{destinations: make(map[string]*DiagnosticExporter)}
	var errs []error
	legacyCfg := cfg
	legacyCfg.Destinations = nil
	legacy, legacyErr := NewDiagnosticExporter(legacyCfg)
	parent.legacy = legacy
	errs = append(errs, legacyErr)
	cfg = prepareNamedDestinations(cfg)
	for _, destination := range cfg.Destinations {
		childCfg := destination.Config
		childCfg.Scrubber = cfg.Scrubber
		childCfg.ServiceVersion, childCfg.BuildCommit = cfg.ServiceVersion, cfg.BuildCommit
		childCfg.ResourceAttributes = append(cfg.ResourceAttributes[:len(cfg.ResourceAttributes):len(cfg.ResourceAttributes)], childCfg.ResourceAttributes...)
		childCfg.AzureMonitorReplayStart = cfg.AzureMonitorReplayStart
		child, err := NewDiagnosticExporter(childCfg)
		if err != nil {
			childCfg.ExporterHealth.RecordUnavailable(err)
			errs = append(errs, fmt.Errorf("destination %s: %w", destination.Name, err))
		}
		if child != nil {
			parent.destinations[destination.Name] = child
			childCfg.ExporterHealth.observeDiagnostics(child.Stats)
		}
	}
	if len(parent.destinations) == 0 {
		return legacy, errors.Join(errs...)
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
	total := d.legacy.Stats()
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
	if d.legacy != nil {
		children = append(children, d.legacy)
	}
	return parallelDestinationCalls(len(children), func(i int) error { return children[i].Shutdown(ctx) })
}
