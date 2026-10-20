package interactiveops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
)

// ErrChildCustodyPending means child cleanup could not be confirmed. Callers
// must retain custody independently of the returned Action's shape or status.
var ErrChildCustodyPending = errors.New("native child custody pending")

// ReviewChildEvent reports the exact bounded definition and observed native
// receipts. Prompt is descriptive metadata and is never interpreted as code.
type ReviewChildEvent struct {
	Kind              string  `json:"kind"`
	Prompt            string  `json:"prompt,omitempty"`
	WorkflowYAML      string  `json:"workflowYaml,omitempty"`
	BoundWorkflowYAML string  `json:"boundWorkflowYaml,omitempty"`
	PayloadDigest     string  `json:"payloadDigest,omitempty"`
	BindingDigest     string  `json:"bindingDigest,omitempty"`
	Action            *Action `json:"action,omitempty"`
}

// ReviewChildOptions binds a child review to its snapshot and execution policy.
type ReviewChildOptions struct {
	// WorkflowYAML is the exact portable reviewed definition. No command is
	// derived from the description. Empty preserves the default review wrapper.
	WorkflowYAML          string
	ApprovedPayloadDigest string
}

// RunReviewChildWithOptions runs the exact approved bounded workflow, binding
// only relative command paths to the child snapshot. It preserves all authored
// stages and the workflow name; the caller owns parent/child authorization.
func (s *Service) RunReviewChildWithOptions(ctx context.Context, scope Scope, requestID, prompt string, options ReviewChildOptions, emit func(ReviewChildEvent) error) (result Action, resultErr error) {
	if strings.TrimSpace(requestID) == "" || len(requestID) > 128 {
		return result, errors.New("child request ID must contain 1–128 characters")
	}
	if len(prompt) > 4096 {
		return result, errors.New("child description exceeds 4096 bytes")
	}
	// A caller without a deadline still gets a bounded capacity-wait + run.
	ctx, cancel := context.WithTimeout(ctx, s.opts.RunTimeout+30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		return result, err
	}
	workflowYAML := options.WorkflowYAML
	if workflowYAML == "" {
		workflowYAML = strings.Replace(state.WorkflowDraft, "name: invoice-check-custom", "name: review-child", 1)
		workflowYAML = strings.Replace(workflowYAML, "name: repository-check-custom", "name: review-child", 1)
	}
	validated, err := s.ValidateWorkflow(ctx, scope, workflowYAML)
	if err != nil {
		return result, err
	}
	if !validated.Valid {
		return result, validationError(validated)
	}
	if options.ApprovedPayloadDigest != "" && (digest([]byte(workflowYAML)) != options.ApprovedPayloadDigest || validated.PayloadDigest != options.ApprovedPayloadDigest) {
		return result, errors.New("child workflow differs from exact approved normalized payload")
	}
	// The stored request binds the exact source bytes, while the event also
	// exposes normalized and execution digests for review/replay verification.

	key := "review-child-" + digest([]byte(requestID))[:32]
	var runID string
	owned := false
	defer func() {
		if resultErr == nil || !owned {
			return
		}
		// The request context may already be cancelled. Cleanup is independently
		// bounded and affects only this exact child scope, never the service root.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
		defer cleanupCancel()
		cleanupErr := s.StopSession(cleanupCtx, scope)
		if runID != "" {
			if latest, err := s.Get(cleanupCtx, scope, runID); err == nil {
				result = latest
			}
		}
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("%w: cleanup unconfirmed: %w", ErrChildCustodyPending, cleanupErr))
		}
	}()
	emitEvent := func(event ReviewChildEvent) error {
		if emit == nil {
			return nil
		}
		return emit(event)
	}
	author, err := s.Propose(ctx, scope, Request{Kind: "add_workflow", WorkflowYAML: workflowYAML, Message: prompt, IdempotencyKey: key + "-definition"})
	if err != nil {
		return result, err
	}
	owned = true
	if err = emitEvent(ReviewChildEvent{Kind: "proposal", Prompt: prompt, WorkflowYAML: validated.NormalizedYAML, BoundWorkflowYAML: validated.BoundYAML, PayloadDigest: validated.PayloadDigest, BindingDigest: validated.BindingDigest, Action: &author}); err != nil {
		return result, err
	}
	author, err = s.Apply(ctx, scope, author.ID)
	if err != nil {
		return result, err
	}
	if err = emitEvent(ReviewChildEvent{Kind: "workflow", Action: &author}); err != nil {
		return result, err
	}
	if author.Status != "succeeded" {
		return author, fmt.Errorf("native child workflow was not applied: %s (%s)", author.Status, author.Error)
	}
	result, err = s.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: validated.Name, Message: prompt, IdempotencyKey: key + "-run"})
	if err != nil {
		return result, err
	}
	runID = result.ID
	fingerprint := ""
	for {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		// An observation error can return a zero Action even though dispatch
		// already happened. Retain the last receipt until a new read succeeds.
		var observed Action
		if result.Status == "proposed" {
			observed, err = s.Apply(ctx, scope, runID)
		} else {
			observed, err = s.Get(ctx, scope, runID)
		}
		if err != nil {
			if !errors.Is(err, lock.ErrHeld) {
				return result, err
			}
		} else {
			result = observed
			snapshot := result
			snapshot.UpdatedAt = time.Time{}
			data, _ := json.Marshal(snapshot)
			next := digest(data)
			if next != fingerprint {
				eventAction := result
				if err = emitEvent(ReviewChildEvent{Kind: "run", Action: &eventAction}); err != nil {
					return result, err
				}
				fingerprint = next
			}
			switch result.Status {
			case "succeeded", "failed", "cancelled":
				if result.CleanupConfirmed {
					return result, nil
				}
				if result.OperationStatus == "failed" {
					return result, fmt.Errorf("native child was not dispatched: %s", result.Error)
				}
			case "uncertain":
				if result.CleanupConfirmed {
					return result, errors.New("native child outcome is unknown; process cleanup confirmed")
				}
				return result, errors.New("native child outcome and cleanup are unconfirmed")
			}
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
