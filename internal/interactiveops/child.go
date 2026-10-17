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
	Kind         string  `json:"kind"`
	Prompt       string  `json:"prompt,omitempty"`
	WorkflowYAML string  `json:"workflowYaml,omitempty"`
	Action       *Action `json:"action,omitempty"`
}

// RunReviewChild authors and executes one test-only native workflow in a scope
// the caller has already admitted as a child. The caller must supply a separate
// immutable repository snapshot through RepositoryForScope (or the ordinary
// per-scope fixture copy). It shares this Service's admission pool. A failed
// test is a normal returned Action; errors mean admission, custody, observation,
// or cancellation failed. No model, provider action, or prompt-derived command
// is supported.
func (s *Service) RunReviewChild(ctx context.Context, scope Scope, requestID, prompt string, emit func(ReviewChildEvent) error) (result Action, resultErr error) {
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
	workflowYAML := strings.Replace(state.WorkflowDraft, "name: invoice-check-custom", "name: review-child", 1)
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
	if err = emitEvent(ReviewChildEvent{Kind: "proposal", Prompt: prompt, WorkflowYAML: workflowYAML, Action: &author}); err != nil {
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
	result, err = s.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "review-child", Message: prompt, IdempotencyKey: key + "-run"})
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
