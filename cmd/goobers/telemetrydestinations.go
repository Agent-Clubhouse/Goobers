package main

import (
	"context"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func configureNamedTelemetry(ctx context.Context, cfg *telemetry.Config, source instance.TelemetryConfig, root string, registry *journal.RegistryScrubber, stores credentials.StoreResolver, diagnostics bool) error {
	constructors := bootstrap.TelemetryDestinationConstructors{
		OTLP: func(ctx context.Context, cfg *telemetry.Config, source instance.OTLPConfig) error {
			return configureOTLP(ctx, cfg, source, registry, stores)
		},
		AzureMonitor: func(ctx context.Context, cfg *telemetry.Config, source instance.AzureMonitorConfig, profile instance.TelemetryCollectionProfile) error {
			return configureAzureMonitor(ctx, cfg, source, profile, root, registry, stores)
		},
	}
	return bootstrap.ConfigureNamedTelemetry(ctx, cfg, source, root, constructors, diagnostics)
}

func telemetryTestConnection(source instance.TelemetryConfig, name string) (instance.TokenRef, error) {
	return bootstrap.TelemetryTestConnection(source, name)
}
