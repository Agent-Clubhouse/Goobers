package runtimewiring

import (
	"context"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// ConfigureNamedTelemetry configures named telemetry destinations from the instance settings.
func ConfigureNamedTelemetry(ctx context.Context, cfg *telemetry.Config, source instance.TelemetryConfig, root string, registry *journal.RegistryScrubber, stores credentials.StoreResolver, diagnostics bool) error {
	constructors := bootstrap.TelemetryDestinationConstructors{
		OTLP: func(ctx context.Context, cfg *telemetry.Config, source instance.OTLPConfig) error {
			return ConfigureOTLP(ctx, cfg, source, registry, stores)
		},
		AzureMonitor: func(ctx context.Context, cfg *telemetry.Config, source instance.AzureMonitorConfig, profile instance.TelemetryCollectionProfile) error {
			return ConfigureAzureMonitor(ctx, cfg, source, profile, root, registry, stores)
		},
	}
	return bootstrap.ConfigureNamedTelemetry(ctx, cfg, source, root, constructors, diagnostics)
}
