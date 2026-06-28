// Package invoke holds the neutral boundary interfaces between the workflow
// engine and the goober runtime. The engine (M7) calls these; the runtime (M8)
// implements them. Keeping them here — depending only on api/v1alpha1 envelope
// types — means neither side imports the other just to share the contract.
package invoke

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Goober is the boundary the runtime implements to execute agentic work. The
// engine builds a canonical invocation envelope and asks for either a task
// result or a reviewer verdict; it knows nothing about how a goober pod is
// prepared or run.
type Goober interface {
	// Invoke runs an agentic task and returns its result envelope.
	Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error)
	// Review runs an agentic reviewer gate and returns its verdict.
	Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error)
}

// Deterministic executes a deterministic (code-driven) task — a separate seam so
// the engine never embeds a process/exec implementation.
type Deterministic interface {
	Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error)
}

// Automated runs a coded check for an automated gate, returning an outcome string
// (e.g. "pass"/"fail") that the gate maps to a branch.
type Automated interface {
	Evaluate(ctx context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error)
}
