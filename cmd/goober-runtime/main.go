// Command goober-runtime is the per-run agent runtime. It executes inside an
// ephemeral run pod: it receives the invocation envelope, drives the harness,
// and signals completion (DEP-004..DEP-007, GBO-011..GBO-013).
package main

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/app"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gooberruntime"
)

func main() {
	app.Main("goober-runtime", func(ctx context.Context, log *slog.Logger) error {
		c, err := client.Dial(client.Options{
			HostPort:  envDefault("GOOBERS_TEMPORAL_ADDRESS", "TEMPORAL_ADDRESS", "localhost:7233"),
			Namespace: envDefault("GOOBERS_TEMPORAL_NAMESPACE", "TEMPORAL_NAMESPACE", "default"),
		})
		if err != nil {
			return err
		}
		defer c.Close()

		rt := gooberruntime.New(gooberruntime.Options{
			Preparer: gooberRuntimePreparer(),
			Harness:  gooberruntime.NewCopilotHarness(commandFromEnv("GOOBERS_COPILOT_HARNESS_COMMAND", "GOOBER_HARNESS_COMMAND")),
		})
		w := worker.New(c, envDefault("GOOBERS_TEMPORAL_TASK_QUEUE", "TEMPORAL_TASK_QUEUE", "goobers"), worker.Options{})
		engine.RegisterWith(w, &engine.Activities{Goober: rt})
		if err := w.Start(); err != nil {
			return err
		}
		log.Info("goober-runtime worker online")
		<-ctx.Done()
		w.Stop()
		return nil
	})
}

func gooberRuntimePreparer() gooberruntime.InProcessPreparer {
	return gooberruntime.InProcessPreparer{
		WorkspaceRoot: envDefault("GOOBERS_WORKSPACE_ROOT", "GOOBER_WORKSPACE_ROOT", ""),
		Providers:     gooberruntime.EnvProviderResolver{},
	}
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
