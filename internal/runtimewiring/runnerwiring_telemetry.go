package runtimewiring

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// ConfigureAzureMonitor configures telemetry export from the instance settings.
func ConfigureAzureMonitor(
	ctx context.Context,
	cfg *telemetry.Config,
	azure instance.AzureMonitorConfig,
	profile instance.TelemetryCollectionProfile,
	instanceRoot string,
	registry *journal.RegistryScrubber,
	stores credentials.StoreResolver,
) error {
	ref := azure.ConnectionString.CredentialTokenRef("telemetry.azureMonitor.connectionString")
	resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{ref}, stores)
	if err != nil {
		return fmt.Errorf("configure Azure Monitor telemetry: %w", err)
	}
	connectionString, err := resolver.Resolve(ctx, ref.Name)
	if err != nil {
		return fmt.Errorf("resolve Azure Monitor connection string: %w", err)
	}
	if registry != nil {
		registry.Register([]byte(connectionString))
	}
	cfg.AzureMonitorConnectionString = connectionString
	cfg.AzureMonitorTraces = profile.IncludesTraces()
	cfg.AzureMonitorJournalLogs = profile.IncludesJournal()
	cfg.AzureMonitorHostIdentity = profile.IncludesHostIdentity()
	if azure.Replay.EnabledEffective() && instanceRoot != "" {
		// #6058: the replay spool and journal-export cursor store open SQLite
		// through sqliteuri.File, whose contract is an absolute path. A
		// relative instance root (`goobers up .`) became "file:///telemetry-
		// export/…" at the filesystem root, so journal catch-up could never
		// open its cursor store and no run journal was exported.
		spoolRoot, err := filepath.Abs(filepath.Join(instanceRoot, "telemetry-export", "azure-monitor"))
		if err != nil {
			return fmt.Errorf("resolve Azure Monitor replay root: %w", err)
		}
		cfg.AzureMonitorReplayRoot = spoolRoot
		cfg.AzureMonitorReplayMaxAge = azure.Replay.MaxAgeDuration()
		cfg.AzureMonitorReplayMaxBytes = azure.Replay.MaxBytesEffective()
	}
	cfg.ResourceAttributes = append(cfg.ResourceAttributes,
		attribute.String("goobers.telemetry.profile", string(profile)))
	return nil
}

// ResolveOTLPHeaders resolves credential references in OTLP request headers.
func ResolveOTLPHeaders(
	ctx context.Context,
	headerRefs map[string]instance.TokenRef,
	registry *journal.RegistryScrubber,
	stores credentials.StoreResolver,
) (map[string]string, error) {
	names := make([]string, 0, len(headerRefs))
	for name := range headerRefs {
		names = append(names, name)
	}
	sort.Strings(names)

	refs := make([]credentials.TokenRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, headerRefs[name].CredentialTokenRef("telemetry.otlp.headers."+strings.ToLower(name)))
	}
	resolver, err := credentials.NewResolverWithStores(refs, stores)
	if err != nil {
		return nil, fmt.Errorf("configure telemetry OTLP headers: %w", err)
	}

	headers := make(map[string]string, len(names))
	for i, name := range names {
		value, err := resolver.Resolve(ctx, refs[i].Name)
		if err != nil {
			return nil, fmt.Errorf("resolve telemetry OTLP header %q: %w", name, err)
		}
		if registry != nil {
			registry.Register([]byte(value))
		}
		headers[name] = value
	}
	return headers, nil
}

// ConfigureOTLP configures the instance OTLP exporter.
func ConfigureOTLP(ctx context.Context, cfg *telemetry.Config, otlp instance.OTLPConfig, registry *journal.RegistryScrubber, stores credentials.StoreResolver) error {
	cfg.JournalLogs = otlp.JournalLogsEnabled()
	if otlp.Enabled() {
		headers, err := ResolveOTLPHeaders(ctx, otlp.Headers, registry, stores)
		if err != nil {
			return err
		}
		cfg.Exporter = telemetry.ExporterOTLP
		cfg.OTLPEndpoint = otlp.Endpoint
		cfg.OTLPInsecure = otlp.Insecure
		cfg.OTLPHeaders = headers
		if otlp.TLS != nil {
			cfg.OTLPCAFile = otlp.TLS.CAFile
			cfg.OTLPServerName = otlp.TLS.ServerName
			cfg.OTLPCertFile = otlp.TLS.CertFile
			cfg.OTLPKeyFile = otlp.TLS.KeyFile
		}
	}
	return nil
}
