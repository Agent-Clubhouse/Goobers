package triggerqueue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func failedRestartChild(t *testing.T, s *Store) (ChildRecord, ChildResult) {
	t.Helper()
	c := acceptChildTest(t, s, childRequest("parent", "stage/visit-1", "child"), childTestTime)
	r := childResultValue("immutable source result", "original workspace")
	if err := s.KeepChildResult(t.Context(), c, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: r.ReceiptDigest}, childTestTime); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetChild(t.Context(), c.Identity)
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}
func childRestartRequest(c ChildRecord, runID string) ChildRestartRequest {
	plan := []byte(`{"selectedGuidance":["guidance-1"],"priorContext":"retained","target":"implement"}`)
	return ChildRestartRequest{Identity: c.Identity, RunID: runID, SourceRunID: c.ActiveRunID(), SourceTerminalSeq: 17, SourceResultRef: c.ResultRef, Actor: "issuer:human", Stage: "implement", Plan: plan, PlanDigest: "sha256:" + childDigest(plan)}
}
func TestChildRestartPreservesOriginalAndFencesStaleResultAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	original, result := failedRestartChild(t, s)
	request := childRestartRequest(original, "human-epoch-1")
	epoch, duplicate, err := s.BeginChildRestart(t.Context(), request, childTestTime.Add(time.Second))
	if err != nil || duplicate || epoch.Epoch != 1 {
		t.Fatal(epoch, duplicate, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	current, err := s.GetChild(t.Context(), original.Identity)
	if err != nil || current.RunID != original.RunID || current.ActiveRunID() != epoch.RunID || current.State != ChildQueued || current.ResultRef != "" {
		t.Fatal(current, err)
	}
	if err = s.KeepChildResult(t.Context(), current, result); !errors.Is(err, ErrConflict) {
		t.Fatal("prior result reused by new execution", err)
	}
	prior, err := s.ChildExecutionResult(t.Context(), original.Identity, original.RunID)
	if err != nil || !reflect.DeepEqual(prior, result) {
		t.Fatal(prior, err)
	}
	if err = s.KeepChildResult(t.Context(), original, result); !errors.Is(err, ErrChildResultUnavailable) {
		t.Fatal("late original capture", err)
	}
	if err = s.SetChildState(t.Context(), original.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: result.ReceiptDigest}, childTestTime.Add(time.Second)); !errors.Is(err, ErrTransition) {
		t.Fatal("late original observation", err)
	}
	if err = s.AcknowledgeChild(t.Context(), original.Identity, result.ReceiptDigest, childTestTime.Add(time.Second)); !errors.Is(err, ErrTransition) {
		t.Fatal("stale result released slot", err)
	}
	assertChildRestartReplayAndLaunch(t, s, original, epoch, request, result)
}

func assertChildRestartReplayAndLaunch(t *testing.T, s *Store, original ChildRecord, epoch ChildExecution, request ChildRestartRequest, result ChildResult) {
	t.Helper()
	var err error
	for _, runID := range []string{original.RunID, epoch.RunID} {
		mapped, err := s.ChildForExecutionRun(t.Context(), runID)
		if err != nil || mapped.Identity != original.Identity || mapped.ActiveRunID() != epoch.RunID {
			t.Fatal(mapped, err)
		}
	}
	if _, err = s.ChildForRun(t.Context(), epoch.RunID); err == nil {
		t.Fatal("execution became accepted identity")
	}
	replay, duplicate, err := s.BeginChildRestart(t.Context(), request, childTestTime.Add(2*time.Second))
	if err != nil || !duplicate || replay.RequestDigest != epoch.RequestDigest {
		t.Fatal(replay, duplicate, err)
	}
	changed := request
	changed.Actor = "issuer:other"
	if _, _, err = s.BeginChildRestart(t.Context(), changed, childTestTime.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("changed actor reused request", err)
	}
	history, err := s.ChildExecutionHistory(t.Context(), original.Identity)
	if err != nil || len(history) != 2 || history[0].RunID != original.RunID || history[0].ResultRef != result.ReceiptDigest || len(history[1].Plan) != 0 {
		t.Fatal(history, err)
	}
	called := false
	if err = s.WithChildResume(t.Context(), original.Identity, func() error { called = true; return nil }); !errors.Is(err, ErrTransition) || called {
		t.Fatal("unbound resume", err)
	}
	if err = s.WithChildExecutionResume(t.Context(), original.Identity, epoch.RunID, func() error { called = true; return nil }); err != nil || !called {
		t.Fatal("current resume", err)
	}
}
func TestChildRestartRaceAdmissionCancellationAndDisposition(t *testing.T) {
	for _, mode := range []string{"other restart", "cancel", "disposition"} {
		t.Run(mode, func(t *testing.T) {
			s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
			c, _ := failedRestartChild(t, s)
			request := childRestartRequest(c, "epoch-a")
			var wg sync.WaitGroup
			start := make(chan struct{})
			results := make(chan error, 2)
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, _, err := s.BeginChildRestart(context.Background(), request, childTestTime.Add(time.Second))
				results <- err
			}()
			go func() {
				defer wg.Done()
				<-start
				var err error
				switch mode {
				case "other restart":
					other := request
					other.RunID = "epoch-b"
					_, _, err = s.BeginChildRestart(context.Background(), other, childTestTime.Add(time.Second))
				case "cancel":
					err = s.FenceChildParent(context.Background(), c.Identity.ChildParent, "human", childTestTime.Add(time.Second))
				case "disposition":
					err = s.AcknowledgeChild(context.Background(), c.Identity, c.ResultRef, childTestTime.Add(time.Second))
				}
				results <- err
			}()
			close(start)
			wg.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				} else if !errors.Is(err, ErrTransition) && !errors.Is(err, ErrParentCancelled) {
					t.Fatal(err)
				}
			}
			current, err := s.GetChild(t.Context(), c.Identity)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				if !current.CancellationRequested {
					t.Fatal("lost cancellation")
				}
				if err = s.WithChildExecutionResume(t.Context(), c.Identity, current.ActiveRunID(), func() error { t.Fatal("cancelled launch"); return nil }); !errors.Is(err, ErrParentCancelled) {
					t.Fatal(err)
				}
			} else if successes != 1 {
				t.Fatal("competing custodians admitted", successes, current)
			}
			if current.ExecutionEpoch > 0 && !current.AcknowledgedAt.IsZero() {
				t.Fatal("restart and old result both consumed")
			}
		})
	}
}
func settleChildEpoch(t *testing.T, s *Store, c ChildRecord, at time.Time) ChildRecord {
	t.Helper()
	r := childResultValue("result "+c.ActiveRunID(), "workspace "+c.ActiveRunID())
	if err := s.KeepChildResult(t.Context(), c, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{ExecutionRunID: c.ActiveRunID(), Expected: ChildQueued, State: ChildFailed, ResultRef: r.ReceiptDigest}, at); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetChild(t.Context(), c.Identity)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestChildRestartHistoryBoundAndProductionFamilyPruning(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c, original := failedRestartChild(t, s)
	id := c.Identity
	for n := 1; n <= MaxChildExecutionEpochs; n++ {
		at := childTestTime.Add(time.Duration(n) * time.Second)
		_, _, err := s.BeginChildRestart(t.Context(), childRestartRequest(c, fmt.Sprint("epoch-", n)), at)
		if err != nil {
			t.Fatal(n, err)
		}
		c, err = s.GetChild(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		c = settleChildEpoch(t, s, c, at)
	}
	if _, _, err := s.BeginChildRestart(t.Context(), childRestartRequest(c, "too-many"), childTestTime.Add(time.Minute)); !errors.Is(err, ErrChildLimit) {
		t.Fatal("history limit", err)
	}
	history, err := s.ChildExecutionHistory(t.Context(), id)
	if err != nil || len(history) != 9 {
		t.Fatal(len(history), err)
	}
	for _, e := range history {
		result, err := s.ChildExecutionResult(t.Context(), id, e.RunID)
		if err != nil || result.ReceiptDigest != e.ResultRef {
			t.Fatal(e, err)
		}
	}
	if original.ReceiptDigest != history[0].ResultRef {
		t.Fatal("original changed")
	}
	now := childTestTime.Add(time.Hour)
	if err = s.MarkChildParentSettled(t.Context(), id.ChildParent, now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PruneChildren(t.Context(), now.Add(ChildRetention+time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if childTableCount(t, s, "child_execution_results") != 8 {
		t.Fatal("unacknowledged history pruned")
	}
	if err = s.AcknowledgeChild(t.Context(), id, c.ResultRef, now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PruneChildren(t.Context(), now.Add(ChildRetention+time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if childTableCount(t, s, "child_execution_results") != 0 || childTableCount(t, s, "child_execution_epochs") != 9 {
		t.Fatal("custody/tombstone retention")
	}
	var retainedBytes int
	if err = s.db.QueryRow(`SELECT SUM(length(plan)) FROM child_execution_epochs`).Scan(&retainedBytes); err != nil || retainedBytes != 0 {
		t.Fatal(retainedBytes, err)
	}
	if _, err = s.ChildForExecutionRun(t.Context(), c.ActiveRunID()); err != nil {
		t.Fatal("tombstone lost execution mapping", err)
	}
	if _, err = s.PruneChildren(t.Context(), now.Add(2*ChildRetention+2*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if childTableCount(t, s, "child_execution_epochs") != 0 {
		t.Fatal("expired history not pruned")
	}
}
func TestChildRestartCancelledParentInventoryAndReplay(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c, _ := failedRestartChild(t, s)
	r := childRestartRequest(c, "epoch")
	if _, _, err := s.BeginChildRestart(t.Context(), r, childTestTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.FenceChildParent(t.Context(), c.Identity.ChildParent, "human", childTestTime.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	page, err := s.PendingChildCancellations(t.Context(), c.Identity.ChildParent, "", 100)
	if err != nil || len(page) != 1 || page[0].ActiveRunID() != r.RunID {
		t.Fatal(page, err)
	}
	if _, duplicate, err := s.BeginChildRestart(t.Context(), r, childTestTime.Add(3*time.Second)); err != nil || !duplicate {
		t.Fatal(duplicate, err)
	}
}
