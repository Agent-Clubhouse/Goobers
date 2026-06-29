package gooberruntime

import (
	"context"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
)

// Options configures a Runtime.
type Options struct {
	Preparer            EnvironmentPreparer
	Harness             Harness
	Evaluator           Evaluator
	InstructionResolver InstructionResolver
	RequireInstructions bool
}

// Runtime implements internal/invoke.Goober for agentic tasks and reviewer
// gates.
type Runtime struct {
	preparer            EnvironmentPreparer
	harness             Harness
	evaluator           Evaluator
	instructionResolver InstructionResolver
	requireInstructions bool
}

var _ invoke.Goober = (*Runtime)(nil)

// New constructs a Runtime.
func New(opts Options) *Runtime {
	return &Runtime{
		preparer:            opts.Preparer,
		harness:             opts.Harness,
		evaluator:           opts.Evaluator,
		instructionResolver: opts.InstructionResolver,
		requireInstructions: opts.RequireInstructions,
	}
}

// Invoke prepares a workspace, invokes the harness, and returns a valid result
// envelope.
func (r *Runtime) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	req, err := r.prepareRequest(ctx, env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if r.harness == nil {
		return apiv1.ResultEnvelope{}, ErrHarnessUnavailable
	}
	result, err := r.harness.Invoke(ctx, req)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("invoke harness: %w", err)
	}
	if err := validateResult(result); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return result, nil
}

// Review prepares a workspace, evaluates the upstream task outputs, and returns
// a valid verdict envelope.
func (r *Runtime) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	req, err := r.prepareRequest(ctx, env)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	evaluator := r.evaluator
	if evaluator == nil {
		evaluator = HarnessEvaluator{Harness: r.harness}
	}
	verdict, err := evaluator.Evaluate(ctx, req)
	if err != nil {
		return apiv1.Verdict{}, fmt.Errorf("evaluate gate: %w", err)
	}
	if err := validateVerdict(verdict); err != nil {
		return apiv1.Verdict{}, err
	}
	return verdict, nil
}

func (r *Runtime) prepareRequest(ctx context.Context, env apiv1.InvocationEnvelope) (HarnessRequest, error) {
	if r.preparer == nil {
		return HarnessRequest{}, fmt.Errorf("environment preparer is required")
	}
	gctx, err := buildContext(ctx, env, r.instructionResolver)
	if err != nil {
		return HarnessRequest{}, fmt.Errorf("build goober context: %w", err)
	}
	if r.requireInstructions && strings.TrimSpace(gctx.Instructions) == "" {
		return HarnessRequest{}, fmt.Errorf("build goober context: instructions are required")
	}
	execEnv, err := r.preparer.Prepare(ctx, env)
	if err != nil {
		return HarnessRequest{}, fmt.Errorf("prepare execution environment: %w", err)
	}
	return HarnessRequest{Context: gctx, Environment: execEnv}, nil
}

func validateResult(result apiv1.ResultEnvelope) error {
	if !result.Status.IsValid() {
		return fmt.Errorf("invalid result status %q", result.Status)
	}
	if result.Status != apiv1.ResultSuccess && result.Error == nil {
		return fmt.Errorf("result status %q requires error detail", result.Status)
	}
	for i, artifact := range result.Artifacts {
		if artifact.Type == "" {
			return fmt.Errorf("artifact[%d].type is required", i)
		}
		if artifact.URI == "" {
			return fmt.Errorf("artifact[%d].uri is required", i)
		}
	}
	return nil
}

func validateVerdict(verdict apiv1.Verdict) error {
	if !verdict.Decision.IsValid() {
		return fmt.Errorf("invalid verdict decision %q", verdict.Decision)
	}
	for i, finding := range verdict.Findings {
		if !validSeverity(finding.Severity) {
			return fmt.Errorf("finding[%d].severity %q is invalid", i, finding.Severity)
		}
		if strings.TrimSpace(finding.Message) == "" {
			return fmt.Errorf("finding[%d].message is required", i)
		}
	}
	return nil
}

func validSeverity(severity apiv1.Severity) bool {
	switch severity {
	case apiv1.SeverityInfo, apiv1.SeverityWarning, apiv1.SeverityError, apiv1.SeverityCritical:
		return true
	default:
		return false
	}
}
