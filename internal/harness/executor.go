package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// Executor is the engine-facing invoke.Goober implementation for agentic
// stages (GBO-051) — checked at compile time so a signature drift is caught
// here, not at the runner's wiring site.
var _ invoke.Goober = (*Executor)(nil)

// SpanRecorder captures a within-stage trace span (GBO-020) — satisfied by
// (*internal/journal.Run).RecordSpan without this package taking on journal's
// full durability/event-log machinery, only its small, stable Ref value type.
type SpanRecorder interface {
	RecordSpan(stage, name string, data []byte) (journal.Ref, error)
}

// Executor adapts one Goober's harness Adapter into the engine-facing
// invoke.Goober seam (GBO-051): the engine only ever sees Invoke/Review;
// harness choice, credential materialization, transcript capture, and the
// completion-contract fail-closed check all happen behind this type. One
// Executor is constructed per Goober (Instructions is goober-level, not
// per-invocation) and reused across its stage invocations.
type Executor struct {
	adapter      Adapter
	injector     *credentials.Injector
	recorder     SpanRecorder
	scrubber     journal.Scrubber
	validator    *validate.Validator
	instructions string
	resultPath   string
	verdictPath  string
	timeout      time.Duration
}

// Option configures an Executor at construction.
type Option func(*Executor)

// WithResultPath overrides the workspace-relative path a task's result JSON
// must be written to (default DefaultResultPath).
func WithResultPath(path string) Option { return func(e *Executor) { e.resultPath = path } }

// WithVerdictPath overrides the workspace-relative path a reviewer gate's
// verdict JSON must be written to (default DefaultVerdictPath).
func WithVerdictPath(path string) Option { return func(e *Executor) { e.verdictPath = path } }

// WithTimeout bounds every harness session this Executor drives.
func WithTimeout(d time.Duration) Option { return func(e *Executor) { e.timeout = d } }

// NewExecutor builds an Executor for one goober: adapter is the harness to
// drive, injector resolves credentials scoped per invocation's declared
// capabilities, recorder captures the (scrubbed) transcript as a journal
// span, scrubber redacts the transcript before it is recorded, and
// instructions is the goober's resolved instructions.md body.
func NewExecutor(adapter Adapter, injector *credentials.Injector, recorder SpanRecorder, scrubber journal.Scrubber, instructions string, opts ...Option) (*Executor, error) {
	if adapter == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil adapter")
	}
	if injector == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil injector")
	}
	if recorder == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil recorder")
	}
	if scrubber == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil scrubber")
	}
	v, err := validate.New()
	if err != nil {
		return nil, fmt.Errorf("harness: init validator: %w", err)
	}
	e := &Executor{
		adapter:      adapter,
		injector:     injector,
		recorder:     recorder,
		scrubber:     scrubber,
		validator:    v,
		instructions: instructions,
		resultPath:   DefaultResultPath,
		verdictPath:  DefaultVerdictPath,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// Invoke implements invoke.Goober: runs the agentic task through the
// configured adapter and returns its result envelope, or an error if the
// stage never produced a valid one (fail closed, GBO-013/GBO-014).
func (e *Executor) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	out, err := e.run(ctx, ModeInvoke, env, e.resultPath)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := e.validator.ValidateEnvelope("result", out.Payload); err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("%w: %w", ErrInvalidCompletion, err)
	}
	var result apiv1.ResultEnvelope
	if err := json.Unmarshal(out.Payload, &result); err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("%w: decode result envelope: %w", ErrInvalidCompletion, err)
	}
	return result, nil
}

// Review implements invoke.Goober: runs an agentic reviewer gate through the
// configured adapter and returns its verdict, or an error if the gate never
// produced a valid one.
func (e *Executor) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	out, err := e.run(ctx, ModeReview, env, e.verdictPath)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	if err := e.validator.ValidateEnvelope("verdict", out.Payload); err != nil {
		return apiv1.Verdict{}, fmt.Errorf("%w: %w", ErrInvalidCompletion, err)
	}
	var verdict apiv1.Verdict
	if err := json.Unmarshal(out.Payload, &verdict); err != nil {
		return apiv1.Verdict{}, fmt.Errorf("%w: decode verdict: %w", ErrInvalidCompletion, err)
	}
	return verdict, nil
}

// run materializes capability-scoped credentials, drives the adapter, and
// records whatever transcript was captured — even on failure, so a runner has
// journaled diagnostics (via the returned error plus the recorded span) beyond
// a bare error string.
func (e *Executor) run(ctx context.Context, mode Mode, env apiv1.InvocationEnvelope, completionPath string) (Outcome, error) {
	creds, err := e.injector.Materialize(ctx, env.Capabilities)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: materialize credentials: %w", err)
	}
	req := RunRequest{
		Mode:           mode,
		Envelope:       env,
		Instructions:   e.instructions,
		Workspace:      env.Workspace,
		CompletionPath: completionPath,
		Credentials:    creds,
		Timeout:        e.timeout,
	}

	out, runErr := e.adapter.Run(ctx, req)
	if len(out.Transcript) > 0 {
		scrubbed := e.scrubber.Scrub(out.Transcript)
		name := fmt.Sprintf("%s.transcript", e.adapter.Name())
		if _, spanErr := e.recorder.RecordSpan(env.TaskID, name, scrubbed); spanErr != nil && runErr == nil {
			runErr = fmt.Errorf("harness: record span: %w", spanErr)
		}
	}
	if runErr != nil {
		return Outcome{}, fmt.Errorf("harness: %s: %w", e.adapter.Name(), runErr)
	}
	if len(out.Payload) == 0 {
		// Defense in depth: an Adapter contract violation (nil error, empty
		// payload) still fails closed rather than surfacing a zero-value
		// result/verdict as a false success.
		return Outcome{}, fmt.Errorf("%w: %s", ErrNoCompletion, completionPath)
	}
	return out, nil
}
