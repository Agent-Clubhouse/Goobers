package interactiveops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
)

// RunReviewChild authors and executes one test-only native workflow in a scope
// the caller has already admitted as a child. The caller must supply a separate
// immutable repository snapshot through RepositoryForScope (or the ordinary
// per-scope fixture copy). It shares this Service's admission pool. A failed
// test is a normal returned Action; errors mean admission, custody, observation,
// or cancellation failed. No model, provider action, or prompt-derived command
// is supported.
func (s *Service) RunReviewChild(ctx context.Context, scope Scope, requestID, prompt string, emit func(ReviewChildEvent) error) (Action, error) {
	return s.RunReviewChildWithOptions(ctx, scope, requestID, prompt, ReviewChildOptions{}, emit)
}

func TestReviewChildCapacityWaitHonorsContextWithoutStoppingParent(t *testing.T) {
	svc, parent := fixture(t, os.Args[0])
	svc.opts.MaxActivePerOwner = 1
	if _, err := svc.Snapshot(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	held, err := lock.TryAcquire(filepath.Join(svc.scopeDir(parent), "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Release(); err != nil {
			t.Error(err)
		}
	}()
	child := Scope{ActorID: parent.ActorID, SessionID: parent.SessionID + "--capacity"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := svc.RunReviewChild(ctx, child, "bounded-wait", "Only run the regression tests", func(event ReviewChildEvent) error {
		if event.Action != nil && event.Action.Waiting == "capacity" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || result.OperationStatus != "prepared" || result.Waiting != "capacity" {
		t.Fatalf("capacity should remain undispatched: %+v %v", result, err)
	}
	if _, err = os.Stat(filepath.Join(svc.scopeDir(parent), "stopped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("child stop fenced its parent")
	}
}

func TestReviewChildObservationFailureRetainsReceiptAndMarksPendingCustody(t *testing.T) {
	svc, scope := fixture(t, os.Args[0])
	ctx := context.Background()
	state, err := svc.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	requestID, prompt := "read-failure", "Only inspect the bounded test results"
	key := "review-child-" + digest([]byte(requestID))[:32]
	workflowYAML := strings.Replace(state.WorkflowDraft, "name: invoice-check-custom", "name: review-child", 1)
	author, err := svc.Propose(ctx, scope, Request{Kind: "add_workflow", WorkflowYAML: workflowYAML, Message: prompt, IdempotencyKey: key + "-definition"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Apply(ctx, scope, author.ID); err != nil {
		t.Fatal(err)
	}
	dispatched, err := svc.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "review-child", Message: prompt, IdempotencyKey: key + "-run"})
	if err != nil {
		t.Fatal(err)
	}
	// Model an existing physical dispatch without starting any subprocess. The
	// held worker lock ensures a healthy first observation; corrupting its ledger
	// afterward makes both subsequent observation and cleanup fail deterministically.
	dispatched.Status = "running"
	dispatched.OperationStatus = "dispatched"
	dispatched.RunID = "known-native-run"
	dispatched.Output = "native submission has already crossed dispatch"
	dir := svc.scopeDir(scope)
	if err = saveAction(dir, &dispatched); err != nil {
		t.Fatal(err)
	}
	worker, err := lock.TryAcquire(filepath.Join(dir, "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := worker.Release(); err != nil {
			t.Error(err)
		}
	}()
	corrupted := false
	result, err := svc.RunReviewChild(ctx, scope, requestID, prompt, func(event ReviewChildEvent) error {
		if event.Kind == "run" && !corrupted {
			corrupted = true
			return os.WriteFile(actionPath(dir, dispatched.ID), []byte("{"), 0600)
		}
		return nil
	})
	if !corrupted || !errors.Is(err, ErrChildCustodyPending) {
		t.Fatalf("cleanup failure must expose typed pending custody: corrupted=%v error=%v", corrupted, err)
	}
	if result.ID != dispatched.ID || result.RunID != dispatched.RunID || result.OperationStatus != "dispatched" || result.Output != dispatched.Output || result.CleanupConfirmed {
		t.Fatalf("lost or fabricated dispatch receipt after observation failure: %+v", result)
	}
}
