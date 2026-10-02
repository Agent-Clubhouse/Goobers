package escalationnotify

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func withDaemonAttributionTask(ctx context.Context, task string) context.Context {
	attribution, ok := providers.AttributionFromContext(ctx)
	if !ok {
		return ctx
	}
	attribution.Task = task
	attribution.Goober = "runner"
	return providers.WithAttributionContext(ctx, attribution)
}

// terminalHandlerAttributionContext attributes a terminal (failed/blocked)
// handler's provider writes to the run it is handling (#5178). The runner
// normally hands over a context that already carries the run's attribution;
// when it does not (a terminal reached before the run attached it, or through
// a path that never did), the run's own durable journal identity supplies it,
// so failure handling is never refused by the guard it exists to satisfy. A
// run with no readable identity keeps the unattributed context and the
// daemon-write guard stays fail-closed.
func (p *Policy) terminalHandlerAttributionContext(ctx context.Context, runID, task string) context.Context {
	if _, ok := providers.AttributionFromContext(ctx); ok {
		return withDaemonAttributionTask(ctx, task)
	}
	if strings.TrimSpace(runID) == "" {
		return ctx
	}
	attributed, err := AttributionContextForRun(ctx, p.RunsDir, runID, task)
	if err != nil {
		return ctx
	}
	return attributed
}

// AttributionContextForRun attributes ctx to runID as the runner performing
// task, from the run's durable journal identity under runsDir. On error it
// returns ctx unchanged alongside the error.
func AttributionContextForRun(ctx context.Context, runsDir, runID, task string) (context.Context, error) {
	reader, err := journal.OpenReadOnly(filepath.Join(runsDir, runID))
	if err != nil {
		return ctx, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return ctx, err
	}
	return providers.WithAttributionContext(ctx, providers.Attribution{
		Schema: 1, Goobers: true,
		Gaggle: identity.Gaggle, Workflow: identity.Workflow,
		Task: task, Goober: "runner", Run: runID,
	}), nil
}
