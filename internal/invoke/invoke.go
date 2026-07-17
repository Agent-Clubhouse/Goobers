// Package invoke holds the neutral boundary interfaces between the workflow
// engine and the goober runtime. The engine (M7) calls these; the runtime (M8)
// implements them. Keeping them here — depending only on api/v1alpha1 envelope
// types — means neither side imports the other just to share the contract.
package invoke

import (
	"context"
	"errors"

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

// InfrastructureError marks a dispatch failure caused by transient external
// infrastructure. The runner applies its bounded infrastructure retry path and
// journals the retry as infrastructure rather than policy-driven.
type InfrastructureError struct {
	err error
}

func (e *InfrastructureError) Error() string { return e.err.Error() }
func (e *InfrastructureError) Unwrap() error { return e.err }

// InfrastructureFailure preserves err while marking it for infrastructure
// retry classification at the runner seam.
func InfrastructureFailure(err error) error {
	if err == nil {
		return nil
	}
	return &InfrastructureError{err: err}
}

// IsInfrastructureFailure reports whether err carries the infrastructure
// marker, including through wrapping.
func IsInfrastructureFailure(err error) bool {
	var infrastructureErr *InfrastructureError
	return errors.As(err, &infrastructureErr)
}

// TimeoutError marks a dispatch failure caused by an agentic session hitting
// its wall-clock timeout. The runtime (internal/harness) tags its own
// process-level timeout with this at the invoke seam so the runner can
// recognize a timeout — and apply a stage's OnTimeout salvage policy (#724) —
// without importing the harness package or matching on error strings.
type TimeoutError struct {
	err error
}

func (e *TimeoutError) Error() string { return e.err.Error() }
func (e *TimeoutError) Unwrap() error { return e.err }

// Timeout preserves err while marking it as a session-timeout failure at the
// runner seam. Returns nil for a nil err.
func Timeout(err error) error {
	if err == nil {
		return nil
	}
	return &TimeoutError{err: err}
}

// IsTimeout reports whether err carries the session-timeout marker, including
// through wrapping.
func IsTimeout(err error) bool {
	var timeoutErr *TimeoutError
	return errors.As(err, &timeoutErr)
}
