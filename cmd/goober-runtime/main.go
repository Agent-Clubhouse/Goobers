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
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gooberruntime"
	"github.com/goobers/goobers/internal/invoke"
)

const defaultTaskQueue = "goobers-engine"

func main() {
	app.Main("goober-runtime", run)
}

type config struct {
	temporalHostPort  string
	temporalNamespace string
	taskQueue         string
	workspaceRoot     string
	harnessCommand    []string
}

func configFromEnv() config {
	return config{
		temporalHostPort:  envDefault("GOOBERS_TEMPORAL_HOSTPORT", "GOOBERS_TEMPORAL_ADDRESS", "TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		temporalNamespace: envDefault("GOOBERS_TEMPORAL_NAMESPACE", "TEMPORAL_NAMESPACE", "default"),
		taskQueue:         envDefault("GOOBERS_TASK_QUEUE", "GOOBERS_TEMPORAL_TASK_QUEUE", "TEMPORAL_TASK_QUEUE", defaultTaskQueue),
		workspaceRoot:     envDefault("GOOBERS_WORKSPACE_ROOT", "GOOBER_WORKSPACE_ROOT", ""),
		harnessCommand:    commandFromEnv("GOOBERS_COPILOT_HARNESS_COMMAND", "GOOBER_HARNESS_COMMAND"),
	}
}

func (c config) validate() error {
	if len(c.harnessCommand) == 0 {
		return errors.New("GOOBERS_COPILOT_HARNESS_COMMAND or GOOBER_HARNESS_COMMAND is required")
	}
	return nil
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg := configFromEnv()
	if err := cfg.validate(); err != nil {
		return err
	}

	rw, err := newWorkerRunner(cfg, newRuntime(cfg))
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

func newRuntime(cfg config) *gooberruntime.Runtime {
	return gooberruntime.New(gooberruntime.Options{
		Preparer: gooberRuntimePreparer(cfg),
		Harness:  gooberruntime.NewCopilotHarness(cfg.harnessCommand),
	})
}

func gooberRuntimePreparer(cfg config) gooberruntime.InProcessPreparer {
	return gooberruntime.InProcessPreparer{
		WorkspaceRoot: cfg.workspaceRoot,
		Providers:     gooberruntime.EnvProviderResolver{},
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

func newTemporalWorkerRunner(cfg config, goober invoke.Goober) (runtimeWorker, error) {
	c, err := client.Dial(client.Options{HostPort: cfg.temporalHostPort, Namespace: cfg.temporalNamespace})
	if err != nil {
		return nil, err
	}
	w := worker.New(c, cfg.taskQueue, worker.Options{})
	registerEngine(w, goober)
	return &temporalWorkerRunner{client: c, worker: w}, nil
}

func registerEngine(w worker.Worker, goober invoke.Goober) {
	engine.RegisterWith(w, &engine.Activities{Goober: goober})
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
