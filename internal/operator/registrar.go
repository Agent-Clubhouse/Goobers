package operator

import (
	"context"
	"log/slog"
)

// WorkflowRegistrar registers a gaggle's workflows with the workflow engine
// (Temporal) so runs can be dispatched against them (DEP-012). The concrete
// implementation is owned by the Workflow Engine mission (M7, Dev-1); the
// operator depends only on this interface and stubs it until that API lands.
type WorkflowRegistrar interface {
	// EnsureRegistered registers (idempotently) the named workflows for a gaggle.
	// It must be safe to call on every reconcile.
	EnsureRegistered(ctx context.Context, gaggle string, workflows []string) error
}

// NoopRegistrar is the default registrar used until the M7 engine registration
// API is available. It records intent via logging and always succeeds, so the
// reconcile loop is fully functional and testable without the engine.
type NoopRegistrar struct {
	Log *slog.Logger
}

// EnsureRegistered logs the workflows that would be registered and returns nil.
func (n NoopRegistrar) EnsureRegistered(_ context.Context, gaggle string, workflows []string) error {
	if n.Log != nil && len(workflows) > 0 {
		n.Log.Debug("workflow registration (noop)", "gaggle", gaggle, "workflows", workflows)
	}
	return nil
}
