package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry"
)

// TelemetryDestinationConstructors supplies the exporter construction adapters.
// Each callback can run concurrently for independently configured destinations.
type TelemetryDestinationConstructors struct {
	OTLP         func(context.Context, *telemetry.Config, instance.OTLPConfig) error
	AzureMonitor func(context.Context, *telemetry.Config, instance.AzureMonitorConfig, instance.TelemetryCollectionProfile) error
}

// ConfigureNamedTelemetry resolves independently so one unavailable secret store cannot consume another
// destination's initialization budget. Diagnostic routing includes only named
// Azure destinations; generic OTLP diagnostics remain separately opt-in.
func ConfigureNamedTelemetry(ctx context.Context, cfg *telemetry.Config, source instance.TelemetryConfig, root string, constructors TelemetryDestinationConstructors, diagnostics bool) error {
	destinations := make([]*telemetry.NamedDestination, len(source.Exporters))
	errs := make([]error, len(source.Exporters))
	var wg sync.WaitGroup
	for i, exporter := range source.Exporters {
		if diagnostics && exporter.Kind != "azuremonitor" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			remote := telemetry.Config{}
			monitor := cfg.ExporterHealth.Destination(exporter.Name, exporter.Kind, exporter.Endpoint)
			if err := configureNamedDestination(resolveCtx, &remote, exporter, source.EffectiveCollectionProfile(), root, constructors); err != nil {
				monitor.RecordUnavailable(err)
				errs[i] = fmt.Errorf("%w: destination %s: %w", telemetry.ErrOTLPUnavailable, exporter.Name, err)
				return
			}
			destinations[i] = &telemetry.NamedDestination{Name: exporter.Name, Config: remote}
		}()
	}
	wg.Wait()
	if !diagnostics && source.NamedAzureEnabled() {
		cfg.ResourceAttributes = append(cfg.ResourceAttributes, attribute.String("goobers.telemetry.profile", string(source.EffectiveCollectionProfile())))
	}
	for _, destination := range destinations {
		if destination != nil {
			cfg.Destinations = append(cfg.Destinations, *destination)
		}
	}
	return errors.Join(errs...)
}

func configureNamedDestination(ctx context.Context, cfg *telemetry.Config, destination instance.TelemetryExporterConfig, profile instance.TelemetryCollectionProfile, root string, constructors TelemetryDestinationConstructors) error {
	switch destination.Kind {
	case "otlp-grpc":
		return constructors.OTLP(ctx, cfg, destination.OTLPConfig())
	case "azuremonitor":
		azure := instance.AzureMonitorConfig{ConnectionString: destination.Connection, Replay: destination.Replay}
		if err := constructors.AzureMonitor(ctx, cfg, azure, profile); err != nil {
			return err
		}
		if cfg.AzureMonitorReplayRoot != "" {
			spoolRoot, err := destination.ReplayRoot(root)
			if err != nil {
				return err
			}
			cfg.AzureMonitorReplayRoot = spoolRoot
		}
		return nil
	default:
		return fmt.Errorf("unsupported telemetry destination kind; supported kinds: otlp-grpc, azuremonitor; see https://github.com/Agent-Clubhouse/Goobers/issues/6502")
	}
}

// TelemetryTestConnection selects the Azure destination for an explicit probe.
func TelemetryTestConnection(source instance.TelemetryConfig, name string) (instance.TokenRef, error) {
	if name == "" && len(source.Exporters) > 0 {
		return instance.TokenRef{}, fmt.Errorf("select a named Azure destination with --destination NAME")
	}
	if name == "" && source.AzureMonitor.Enabled() {
		return source.AzureMonitor.ConnectionString, nil
	}
	for _, destination := range source.Exporters {
		if destination.Name == name {
			if destination.Kind != "azuremonitor" {
				return instance.TokenRef{}, fmt.Errorf("connectivity probes support azuremonitor destinations; inspect collector delivery health for otlp-grpc")
			}
			return destination.Connection, nil
		}
	}
	return instance.TokenRef{}, fmt.Errorf("direct Azure Monitor destination is not configured")
}
