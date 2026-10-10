package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ErrorCodeInvalidPublication names a success completion whose declared,
// schema-bound output is missing or does not satisfy its contract (#6868).
const ErrorCodeInvalidPublication = "INVALID_PUBLICATION"

// ErrInvalidPublication marks a success completion whose schema-bound
// artifact-set publication would be rejected at lift. Like
// ErrUncommittedChanges it is a completion postcondition, not an invalid
// completion: adapters spend their single bounded same-session repair turn on
// it (same thread, same attempt, remaining timeout), and if the repair does
// not fix it the original completion stands so liftArtifacts reports the
// last validation reason as the terminal producer failure.
var ErrInvalidPublication = errors.New(ErrorCodeInvalidPublication + ": the stage reported success but its declared output was not accepted")

// publicationPostcondition is armed for an invoke whose publication contract
// binds at least one slot to a JSON Schema.
type publicationPostcondition struct{}

// armPostconditions hands goobers-io the declared publication schemas and arms
// both completion postconditions; the publication check wraps the commit check
// so one repair turn can address both.
func (e *Executor) armPostconditions(ctx context.Context, mode Mode, env apiv1.InvocationEnvelope, req *RunRequest) (*commitPostcondition, *publicationPostcondition) {
	req.PublicationSchemas = publicationSchemasFor(ctx, env)
	commit := armCommitPostcondition(ctx, mode, env, req)
	return commit, e.armPublicationPostcondition(ctx, mode, env, req)
}

// armPublicationPostcondition wraps req's completion validator so a success
// completion is checked against exactly what liftArtifacts will accept. It
// returns nil — no check, existing behavior — when the stage declares no
// schema-bound slot.
func (e *Executor) armPublicationPostcondition(ctx context.Context, mode Mode, env apiv1.InvocationEnvelope, req *RunRequest) *publicationPostcondition {
	if mode != ModeInvoke || len(declaredPublicationSchemas(ctx, env)) == 0 {
		return nil
	}
	manifest, ok := env.Inputs[InputArtifactManifestFile].(string)
	if !ok || manifest == "" {
		return nil
	}
	base := req.ValidateCompletion
	req.ValidateCompletion = func(payload []byte) error {
		var commitErr error
		if base != nil {
			if err := base(payload); err != nil {
				if !errors.Is(err, ErrUncommittedChanges) {
					return err
				}
				// Check the publication too, so the single repair turn
				// addresses both postconditions instead of only the commit.
				commitErr = err
			}
		}
		if !claimsSuccess(payload) {
			return commitErr
		}
		err := e.checkDeclaredPublication(ctx, env, manifest)
		if err == nil {
			return commitErr
		}
		// One %w keeps this a single member, so withoutPostcondition strips it whole.
		publicationErr := fmt.Errorf("%w: %s", ErrInvalidPublication, err.Error())
		if commitErr == nil {
			return publicationErr
		}
		return errors.Join(commitErr, publicationErr)
	}
	return &publicationPostcondition{}
}

// checkDeclaredPublication runs the lift-time validation without recording
// anything. Only failures liftArtifacts would report as a declared-artifact
// failure are repairable; anything else (I/O, cancellation) is left for the
// authoritative lift to surface.
func (e *Executor) checkDeclaredPublication(ctx context.Context, env apiv1.InvocationEnvelope, manifest string) error {
	_, err := e.prepareDeclaredSet(ctx, env, manifest)
	if _, _, ok := declaredArtifactFailure(err); err != nil && ok {
		return err
	}
	return nil
}

// settle clears an adapter error that is only the publication postcondition:
// the completion itself is valid, so liftArtifacts judges the publication and
// reports the typed declared-artifact failure. Other failures are kept.
func (p *publicationPostcondition) settle(out Outcome, err error) (Outcome, error) {
	if p == nil {
		return out, err
	}
	return out, withoutPostcondition(err, ErrInvalidPublication)
}

func claimsSuccess(payload []byte) bool {
	var claimed struct {
		Status apiv1.ResultStatus `json:"status"`
	}
	return json.Unmarshal(payload, &claimed) == nil && claimed.Status == apiv1.ResultSuccess
}

// isCompletionPostcondition reports whether err is a schema-valid completion
// that failed only a runner-checked postcondition. Such a completion gets the
// adapter's bounded repair turn but is never recorded as an invalid completion.
func isCompletionPostcondition(err error) bool {
	return errors.Is(err, ErrUncommittedChanges) || errors.Is(err, ErrInvalidPublication)
}

// postconditionRepairProblem describes every completion postcondition err
// carries, so a completion that failed both gets one repair turn for both.
func postconditionRepairProblem(err error) (string, bool) {
	var problems []string
	if errors.Is(err, ErrUncommittedChanges) {
		problems = append(problems, uncommittedChangesRepairProblem(postconditionMember(err, ErrUncommittedChanges)))
	}
	if errors.Is(err, ErrInvalidPublication) {
		problems = append(problems, invalidPublicationRepairProblem(postconditionMember(err, ErrInvalidPublication)))
	}
	return strings.Join(problems, " "), len(problems) > 0
}

// postconditionMember returns the member of a joined err that carries target,
// so each repair problem quotes only its own reason.
func postconditionMember(err, target error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, member := range joined.Unwrap() {
			if errors.Is(member, target) {
				return postconditionMember(member, target)
			}
		}
	}
	return err
}

// invalidPublicationRepairProblem opens the repair turn for #6868.
func invalidPublicationRepairProblem(validationErr error) string {
	return fmt.Sprintf(
		"Your previous turn reported success, but its declared output was not accepted (%s). "+
			"Nothing was published. Correct the listed payload file(s) and call `publish_output` again with the "+
			"complete staging manifest; do not repeat completed external actions. This repair turn shares the "+
			"session's remaining budget, and an output that is still invalid fails the stage.",
		validationErr,
	)
}
