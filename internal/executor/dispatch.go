package executor

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// InputKind is the env.Inputs key that selects which built-in deterministic
// stage kind to run. Its absence means KindShell — the common case.
const InputKind = "kind"

// KindShell and KindCIPoll are the built-in deterministic-stage kinds
// TaskExecutor dispatches. KindShell is the implicit default.
const (
	KindShell  = "shell"
	KindCIPoll = "ci-poll"
)

// TaskExecutor implements invoke.Deterministic and is the single dispatcher a
// caller registers for apiv1.TaskDeterministic: the runner constructs one
// invoke.Deterministic per run (internal/runner's NewDeterministic factory),
// so every built-in deterministic-stage kind — the shell executor and the
// ci-poll task alike — has to be reachable through that one entry point,
// selected by env.Inputs[InputKind].
type TaskExecutor struct {
	Shell *ShellExecutor
	// CIPoll may be nil if this instance has no PR provider configured; a
	// stage that declares kind=ci-poll then fails closed rather than
	// silently falling through to the shell executor.
	CIPoll *CIPollExecutor
}

// NewTaskExecutor returns a TaskExecutor over shell and the given
// CIPollExecutor (nil is valid — see CIPoll's doc).
func NewTaskExecutor(shell *ShellExecutor, ciPoll *CIPollExecutor) (*TaskExecutor, error) {
	if shell == nil {
		return nil, errors.New("executor: shell executor must not be nil")
	}
	return &TaskExecutor{Shell: shell, CIPoll: ciPoll}, nil
}

// Run implements invoke.Deterministic, dispatching to the shell or ci-poll
// executor per env.Inputs[InputKind].
func (t *TaskExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	switch stringInput(env, InputKind) {
	case "", KindShell:
		return t.Shell.Run(ctx, env, run)
	case KindCIPoll:
		if t.CIPoll == nil {
			return apiv1.ResultEnvelope{}, errors.New("executor: kind=ci-poll declared but no CIPollExecutor is configured")
		}
		cfg, err := CIPollConfigFromEnvelope(env)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		return t.CIPoll.Run(ctx, cfg)
	default:
		return apiv1.ResultEnvelope{}, errors.New("executor: unknown " + InputKind + " " + stringInput(env, InputKind))
	}
}
