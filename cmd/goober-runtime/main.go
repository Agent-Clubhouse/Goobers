// Command goober-runtime is the per-run agent runtime. It executes inside an
// ephemeral run pod: it receives the invocation envelope, drives the harness,
// and signals completion (DEP-004..DEP-007, GBO-011..GBO-013).
//
// Superseded — folds into the local runner's stage execution (the `goobers`
// binary); kept compiling as the tier-3 agent-pod reference. See
// docs/ARCHITECTURE.md §11.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/app"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/gooberruntime"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/providers"
)

func main() {
	secretReg, scrubber := journal.DefaultScrubber()
	app.MainWithScrubber("goober-runtime", scrubber, func(ctx context.Context, log *slog.Logger) error {
		return runWithScrubber(ctx, log, secretReg, scrubber)
	})
}

type config struct {
	temporalHostPort  string
	temporalNamespace string
	taskQueue         string
	workspaceRoot     string
	harnessCommand    []string
	exporter          telemetry.ExporterKind
	otlpEndpoint      string
	environment       string
}

func configFromEnv() (config, error) {
	engineConfig, err := runtimeEngineConfig(os.Getenv("GOOBERS_INSTANCE_ROOT"))
	if err != nil {
		return config{}, err
	}
	return config{
		temporalHostPort:  engineConfig.HostPort,
		temporalNamespace: engineConfig.Namespace,
		taskQueue:         engineConfig.TaskQueue,
		workspaceRoot:     envDefault("GOOBERS_WORKSPACE_ROOT", "GOOBER_WORKSPACE_ROOT", ""),
		harnessCommand:    commandFromEnv("GOOBERS_COPILOT_HARNESS_COMMAND", "GOOBER_HARNESS_COMMAND"),
		exporter:          telemetry.ExporterKind(envDefault("GOOBERS_OTEL_EXPORTER", string(telemetry.ExporterStdout))),
		otlpEndpoint:      os.Getenv("GOOBERS_OTLP_ENDPOINT"),
		environment:       os.Getenv("GOOBERS_ENV"),
	}, nil
}

func runtimeEngineConfig(root string) (instance.EngineConfig, error) {
	if root != "" {
		cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			return instance.EngineConfig{}, err
		}
		return cfg.EffectiveEngineConfig(), nil
	}
	resolved, _, err := (&instance.Config{}).ResolveEngineConfig(os.LookupEnv)
	return resolved, err
}

func (c config) validate() error {
	if len(c.harnessCommand) == 0 {
		return errors.New("GOOBERS_COPILOT_HARNESS_COMMAND or GOOBER_HARNESS_COMMAND is required")
	}
	return nil
}

func run(ctx context.Context, log *slog.Logger) error {
	secretReg, scrubber := journal.DefaultScrubber()
	return runWithScrubber(ctx, log, secretReg, scrubber)
}

func runWithScrubber(ctx context.Context, log *slog.Logger, secretReg *journal.RegistryScrubber, scrubber journal.Scrubber) error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}

	tel, err := telemetry.New(ctx, runtimeTelemetryConfig(cfg, scrubber))
	if err != nil {
		return err
	}
	defer func() { _ = tel.Shutdown(context.Background()) }()

	rw, err := newWorkerRunner(cfg, newRuntime(cfg, secretReg, scrubber, tel), scrubber)
	if err != nil {
		return err
	}
	if err := rw.Start(); err != nil {
		rw.Close()
		return err
	}
	log.Info("goober-runtime worker online", "taskQueue", cfg.taskQueue)
	<-ctx.Done()
	rw.Stop()
	return nil
}

func newRuntime(cfg config, secretReg *journal.RegistryScrubber, scrubber journal.Scrubber, rateObserver providers.RateLimitObserver) *gooberruntime.Runtime {
	return gooberruntime.New(gooberruntime.Options{
		Preparer:       gooberRuntimePreparer(cfg, secretReg, rateObserver),
		Harness:        gooberruntime.NewCopilotHarness(cfg.harnessCommand),
		OutputScrubber: scrubber,
	})
}

func gooberRuntimePreparer(cfg config, secretReg *journal.RegistryScrubber, rateObserver providers.RateLimitObserver) gooberruntime.InProcessPreparer {
	return gooberruntime.InProcessPreparer{
		WorkspaceRoot: cfg.workspaceRoot,
		Providers: gooberruntime.EnvProviderResolver{
			SecretRegistrar:   secretReg,
			RateLimitObserver: rateObserver,
		},
	}
}

func runtimeTelemetryConfig(cfg config, scrubber journal.Scrubber) telemetry.Config {
	return telemetry.Config{
		ServiceName:    "goober-runtime",
		ServiceVersion: version.Get().Version,
		BuildCommit:    version.Get().Commit,
		Environment:    cfg.environment,
		Exporter:       cfg.exporter,
		OTLPEndpoint:   cfg.otlpEndpoint,
		OTLPInsecure:   cfg.otlpEndpoint != "",
		Batch:          true,
		Scrubber:       scrubber,
	}
}

type runtimeWorker interface {
	Start() error
	Stop()
	Close()
}

var newWorkerRunner = newTemporalWorkerRunner

type temporalWorkerRunner struct {
	client client.Client
	worker worker.Worker
	once   sync.Once
}

func newTemporalWorkerRunner(cfg config, goober invoke.Goober, scrubber journal.Scrubber) (runtimeWorker, error) {
	c, err := client.Dial(client.Options{HostPort: cfg.temporalHostPort, Namespace: cfg.temporalNamespace})
	if err != nil {
		return nil, err
	}
	w := worker.New(c, cfg.taskQueue, worker.Options{})
	registerEngine(w, c, goober, scrubber)
	return &temporalWorkerRunner{client: c, worker: w}, nil
}

func registerEngine(w worker.Worker, temporalClient client.Client, goober invoke.Goober, scrubber journal.Scrubber) {
	bootstrap.RegisterEngine(w, temporalClient, bootstrap.EngineDeps{Goober: goober, Scrubber: scrubber})
}

func (r *temporalWorkerRunner) Start() error {
	return r.worker.Start()
}

func (r *temporalWorkerRunner) Stop() {
	r.worker.Stop()
	r.Close()
}

func (r *temporalWorkerRunner) Close() {
	r.once.Do(r.client.Close)
}

func commandFromEnv(keys ...string) []string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return strings.Fields(value)
		}
	}
	return nil
}

func envDefault(keysAndDefault ...string) string {
	fallback := keysAndDefault[len(keysAndDefault)-1]
	for _, key := range keysAndDefault[:len(keysAndDefault)-1] {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return fallback
}
