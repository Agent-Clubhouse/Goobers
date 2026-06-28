package engine

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
)

// Activity names. The workflow refers to activities by these names so it is
// decoupled from the concrete receiver instance; they must equal the method
// names on Activities exactly (Temporal registers struct methods by name).
const (
	ActInvokeGoober      = "InvokeGoober"
	ActReviewGoober      = "ReviewGoober"
	ActRunDeterministic  = "RunDeterministic"
	ActEvaluateAutomated = "EvaluateAutomated"
)

// Activities bundles the engine's side-effecting operations as Temporal
// activities. Each seam (defined in package invoke) is optional; a nil seam
// yields a clear "not configured" error if the workflow reaches a node that needs
// it, rather than a panic. The runtime (M8) constructs this with a real
// invoke.Goober.
type Activities struct {
	Goober invoke.Goober
	Det    invoke.Deterministic
	Auto   invoke.Automated
}

// ErrNotConfigured is returned by an activity whose backing seam was not wired.
var ErrNotConfigured = errors.New("engine: activity dependency not configured")

// InvokeGoober executes an agentic task.
func (a *Activities) InvokeGoober(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if a.Goober == nil {
		return apiv1.ResultEnvelope{}, ErrNotConfigured
	}
	return a.Goober.Invoke(ctx, env)
}

// ReviewGoober executes an agentic reviewer gate.
func (a *Activities) ReviewGoober(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	if a.Goober == nil {
		return apiv1.Verdict{}, ErrNotConfigured
	}
	return a.Goober.Review(ctx, env)
}

// RunDeterministic executes a deterministic task.
func (a *Activities) RunDeterministic(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if a.Det == nil {
		return apiv1.ResultEnvelope{}, ErrNotConfigured
	}
	return a.Det.Run(ctx, env, run)
}

// EvaluateAutomated runs an automated gate check.
func (a *Activities) EvaluateAutomated(ctx context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	if a.Auto == nil {
		return "", ErrNotConfigured
	}
	return a.Auto.Evaluate(ctx, gate, env)
}
