package launchreceipt

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
)

// ErrLocalPersistence deliberately excludes host paths and transport details.
var ErrLocalPersistence = errors.New("local launch receipt unavailable; execution refused before launch")

// ErrControllerKey refuses all current local execution kinds on a key-holding
// host. No current builtin has a proven transitive non-exec contract.
var ErrControllerKey = errors.New("local/self execution refused: api.podTokenKeyFile is configured; move the signing key off this execution host or use an isolated runner without the key; run goobers doctor --harness-auth or goobers status for guidance")

// RecordLocalInvocation persists only runtime-owned facts against the exact
// controller start. A configured recorder is mandatory at production wiring.
func RecordLocalInvocation(ctx context.Context, recorder Recorder, env apiv1.InvocationEnvelope, review bool, facts LocalFacts) error {
	binding, err := ForInvocation(ctx, env, review)
	if err != nil || recorder == nil {
		return ErrLocalPersistence
	}
	if err := recorder.Record(ctx, Receipt{Version: 1, Binding: binding, Local: &facts}); err != nil {
		return ErrLocalPersistence
	}
	return nil
}

// GuardDeterministic includes in-process builtins as well as shell execution.
// A configured controller key refuses every currently registered kind.
func GuardDeterministic(next invoke.Deterministic, recorder Recorder, controllerKey bool) invoke.Deterministic {
	return guardedDeterministic{next: next, recorder: recorder, controllerKey: controllerKey}
}

type guardedDeterministic struct {
	next          invoke.Deterministic
	recorder      Recorder
	controllerKey bool
}

func (g guardedDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if g.controllerKey {
		return apiv1.ResultEnvelope{}, ErrControllerKey
	}
	if err := RecordLocalInvocation(ctx, g.recorder, env, false, PreparedLocal("deterministic")); err != nil {
		return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(err)
	}
	return g.next.Run(ctx, env, run)
}
