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

// TaskExecutor is the single dispatcher for apiv1.TaskDeterministic: since a
// runner can register only one executor per apiv1.TaskType, every built-in
// deterministic-stage kind (issue #18's "backlog-query" sibling aside — see
// doc.go) is reached through this one entry point, selected by
// env.Inputs[InputKind].
type TaskExecutor struct {
	Shell *ShellExecutor
	// CIPoll may be nil if this instance has no PR provider configured; a
	// stage that declares kind=ci-poll then fails closed rather than
	// silently falling through to the shell executor.
	CIPoll *CIPollExecutor
}

// NewTaskExecutor returns a TaskExecutor with a default ShellExecutor and the
// given CIPollExecutor (nil is valid — see CIPoll's doc).
func NewTaskExecutor(ciPoll *CIPollExecutor) *TaskExecutor {
	return &TaskExecutor{Shell: NewShellExecutor(), CIPoll: ciPoll}
}

// Run dispatches env/run to the shell or ci-poll executor per
// env.Inputs[InputKind], and returns that executor's result plus any produced
// artifacts (always empty for ci-poll, which produces no artifacts).
func (t *TaskExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun, tokens TokenSource) (apiv1.ResultEnvelope, []ProducedArtifact, error) {
	switch stringInput(env, InputKind) {
	case "", KindShell:
		cfg, err := ConfigFromEnvelope(env, run)
		if err != nil {
			return apiv1.ResultEnvelope{}, nil, err
		}
		return t.Shell.Run(ctx, env.Workspace, env.Capabilities, tokens, cfg)
	case KindCIPoll:
		if t.CIPoll == nil {
			return apiv1.ResultEnvelope{}, nil, errors.New("executor: kind=ci-poll declared but no CIPollExecutor is configured")
		}
		cfg, err := CIPollConfigFromEnvelope(env)
		if err != nil {
			return apiv1.ResultEnvelope{}, nil, err
		}
		result, err := t.CIPoll.Run(ctx, cfg)
		return result, nil, err
	default:
		return apiv1.ResultEnvelope{}, nil, errors.New("executor: unknown " + InputKind + " " + stringInput(env, InputKind))
	}
}
