package main

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
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/version"
)

// buildTelemetryClient constructs the OTel client that spans the runner walk
// (run/task/gate) and scheduler decisions, writing completed spans under
// RunsDir via JournalSpanExporter (issue #126) — the same run journal
// layout goobers trace/telemetry read back through the rollup. Shared by
// up.go/run.go exactly like buildRunnerConfig; each caller owns calling
// Shutdown on the returned client once it's done driving runs.
func buildTelemetryClient(
	ctx context.Context,
	l instance.Layout,
	scrubber journal.Scrubber,
	registry *journal.RegistryScrubber,
	telemetryConfig instance.TelemetryConfig,
	stores credentials.StoreResolver,
	// Nil keeps one-shot setup immediate; the daemon supplies its readiness signal.
	replayStart <-chan struct{},
) (*telemetry.Client, error) {
	cfg := telemetry.Config{
		ServiceName:             "goobers",
		ServiceVersion:          version.Get().Version,
		BuildCommit:             version.Get().Commit,
		SpanExporter:            telemetry.NewPerGaggleJournalSpanExporter(l.Root, scrubber),
		Scrubber:                scrubber,
		Batch:                   true,
		JournalRoot:             l.Root,
		AzureMonitorReplayStart: replayStart,
	}
	// Only the durable identity is trustworthy; legacy roots remain unidentified.
	// Carry it on every signal as a resource attribute so operators can correlate
	// process restarts with the same customer-managed instance and its journals.
	cfg.JournalInstanceID, cfg.ResourceAttributes = telemetryInstanceIdentities(l.Root)
	if telemetryConfig.OTLP != nil {
		if err := configureOTLP(ctx, &cfg, *telemetryConfig.OTLP, registry, stores); err != nil {
			return nil, err
		}
	}
	if telemetryConfig.AzureMonitor != nil {
		if err := configureAzureMonitor(ctx, &cfg, *telemetryConfig.AzureMonitor, telemetryConfig.EffectiveCollectionProfile(), l.Root, registry, stores); err != nil {
			return nil, err
		}
	}
	// telemetry.New may return a non-nil *Client alongside an error wrapping
	// telemetry.ErrOTLPUnavailable (invalid TLS material) — that Client is
	// still usable for local-only telemetry, so callers must not treat every
	// non-nil error here as a construction failure. See daemon.go's call
	// site for the degrade handling.
	return telemetry.New(ctx, cfg)
}

func configureAzureMonitor(
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

func resolveOTLPHeaders(
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

// teeRegistrar forwards every registered secret to BOTH a run's own
// SecretRegistrar (feeding that run's journal scrubber) and the instance-global
// shared registry (feeding the span exporter + instance log). It is how a
// per-run secret reaches the two instance-lifetime consumers without changing
// internal/runner's per-run registrar creation (#117 Piece B).
type teeRegistrar struct {
	run    runner.SecretRegistrar
	shared *journal.RegistryScrubber
}

func (t teeRegistrar) Register(secret []byte) {
	t.run.Register(secret)
	t.shared.Register(secret)
}

func configureOTLP(ctx context.Context, cfg *telemetry.Config, otlp instance.OTLPConfig, registry *journal.RegistryScrubber, stores credentials.StoreResolver) error {
	cfg.JournalLogs = otlp.JournalLogsEnabled()
	if otlp.Enabled() {
		headers, err := resolveOTLPHeaders(ctx, otlp.Headers, registry, stores)
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
