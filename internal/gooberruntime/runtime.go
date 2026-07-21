package gooberruntime

import (
	"context"
	"errors"
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
	OutputScrubber      OutputScrubber
}

// OutputScrubber removes secrets before runtime results leave the activity.
type OutputScrubber interface {
	Scrub([]byte) []byte
}

type scrubbedError struct {
	message string
	matches func(error) bool
}

func (e *scrubbedError) Error() string {
	return e.message
}

// Is preserves classification without exposing the secret-bearing original
// through errors.Unwrap.
func (e *scrubbedError) Is(target error) bool {
	return e.matches(target)
}

// Runtime implements internal/invoke.Goober for agentic tasks and reviewer
// gates.
type Runtime struct {
	preparer            EnvironmentPreparer
	harness             Harness
	evaluator           Evaluator
	instructionResolver InstructionResolver
	requireInstructions bool
	outputScrubber      OutputScrubber
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
		outputScrubber:      opts.OutputScrubber,
	}
}

// Invoke prepares a workspace, invokes the harness, and returns a valid result
// envelope.
func (r *Runtime) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (result apiv1.ResultEnvelope, err error) {
	defer func() { err = r.scrubError(err) }()
	req, err := r.prepareRequest(ctx, env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if r.harness == nil {
		return apiv1.ResultEnvelope{}, ErrHarnessUnavailable
	}
	result, err = r.harness.Invoke(ctx, req)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("invoke harness: %w", err)
	}
	r.scrubResult(&result)
	if err := validateResult(result); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return result, nil
}

// Review prepares a workspace, evaluates the upstream task outputs, and returns
// a valid verdict envelope.
func (r *Runtime) Review(ctx context.Context, env apiv1.InvocationEnvelope) (verdict apiv1.Verdict, err error) {
	defer func() { err = r.scrubError(err) }()
	req, err := r.prepareRequest(ctx, env)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	evaluator := r.evaluator
	if evaluator == nil {
		evaluator = HarnessEvaluator{Harness: r.harness}
	}
	verdict, err = evaluator.Evaluate(ctx, req)
	if err != nil {
		return apiv1.Verdict{}, fmt.Errorf("evaluate gate: %w", err)
	}
	r.scrubVerdict(&verdict)
	if err := validateVerdict(verdict); err != nil {
		return apiv1.Verdict{}, err
	}
	return verdict, nil
}

func (r *Runtime) scrubResult(result *apiv1.ResultEnvelope) {
	result.Summary = r.scrubString(result.Summary)
	if result.Error != nil {
		result.Error.Message = r.scrubString(result.Error.Message)
	}
	for key, value := range result.Outputs {
		if text, ok := value.(string); ok {
			result.Outputs[key] = r.scrubString(text)
		}
	}
}

func (r *Runtime) scrubVerdict(verdict *apiv1.Verdict) {
	verdict.Rationale = r.scrubString(verdict.Rationale)
	verdict.Summary = r.scrubString(verdict.Summary)
	for i := range verdict.Findings {
		verdict.Findings[i].Message = r.scrubString(verdict.Findings[i].Message)
		verdict.Findings[i].Location = r.scrubString(verdict.Findings[i].Location)
	}
}

func (r *Runtime) scrubError(err error) error {
	if err == nil || r.outputScrubber == nil {
		return err
	}
	message := r.scrubString(err.Error())
	if message == err.Error() {
		return err
	}
	var scrubbed error = &scrubbedError{
		message: message,
		matches: func(target error) bool {
			return errors.Is(err, target)
		},
	}
	// The scrubbed error deliberately has no Unwrap, so errors.As cannot reach
	// the secret-bearing original. Re-apply the classification markers the
	// runner seam matches on (invoke.IsTimeout, invoke.IsInfrastructureFailure)
	// around the scrubbed message so retry and OnTimeout salvage policy still
	// see a typed failure.
	if invoke.IsInfrastructureFailure(err) {
		scrubbed = invoke.InfrastructureFailure(scrubbed)
	}
	if invoke.IsTimeout(err) {
		scrubbed = invoke.Timeout(scrubbed)
	}
	return scrubbed
}

func (r *Runtime) scrubString(value string) string {
	if r.outputScrubber == nil || value == "" {
		return value
	}
	return string(r.outputScrubber.Scrub([]byte(value)))
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
	if result.Status == apiv1.ResultFailure && result.Error == nil {
		return fmt.Errorf("result status %q requires error detail", result.Status)
	}
	for i, artifact := range result.Artifacts {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("artifact[%d]: %w", i, err)
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
		if !finding.IsValid() {
			return fmt.Errorf("finding[%d] is invalid (class %q, blockingPRs %v)", i, finding.Class, finding.BlockingPRs)
		}
	}
	return nil
}

// ValidateMergeReviewVerdict applies the ordinary verdict rules and requires
// every finding to carry a routing class.
func ValidateMergeReviewVerdict(verdict apiv1.Verdict) error {
	if err := validateVerdict(verdict); err != nil {
		return err
	}
	for i, finding := range verdict.Findings {
		if !finding.Class.IsValid() {
			return fmt.Errorf("finding[%d].class %q is invalid", i, finding.Class)
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
