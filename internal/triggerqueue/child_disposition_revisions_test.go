package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestChildDispositionRevisionsCASAndPublishedPlanFence(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	child, request := dispositionFixture(t, s, "parent", childTestTime)
	request.Action = "merge"
	first, err := s.RequestChildDisposition(t.Context(), request, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Action = "discard"
	if _, err := s.RequestChildDisposition(t.Context(), changed, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal("unconditional replacement", err)
	}
	changed.ExpectedRequestDigest = first.RequestDigest()
	second, err := s.RequestChildDisposition(t.Context(), changed, childTestTime.Add(time.Second))
	if err != nil || second.Action != "discard" || second.PreviousDigest != first.RequestDigest() {
		t.Fatal(second, err)
	}
	retry, err := s.RequestChildDisposition(t.Context(), changed, childTestTime.Add(2*time.Second))
	if err != nil || retry.RequestDigest() != second.RequestDigest() {
		t.Fatal("lost response changed revision", retry, err)
	}
	if _, err := s.RequestChildDisposition(t.Context(), request, childTestTime.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("stale original request replaced latest", err)
	}
	if err := s.KeepChildDispositionPlan(t.Context(), first, []byte("stale plan")); !errors.Is(err, ErrConflict) {
		t.Fatal("old preparation survived revised intent", err)
	}
	if err := s.KeepChildDispositionPlan(t.Context(), second, []byte("exact plan")); err != nil {
		t.Fatal(err)
	}
	changed.Action = "replace"
	changed.ExpectedRequestDigest = second.RequestDigest()
	if _, err := s.RequestChildDisposition(t.Context(), changed, childTestTime.Add(3*time.Second)); !errors.Is(err, ErrChildDispositionReconcile) {
		t.Fatal("published plan replaced", err)
	}
	current, err := s.ChildDisposition(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	nextAuthority := request.Authority
	nextAuthority.GrantID = "replacement-grant"
	nextAuthority.AttemptID = "replacement-attempt"
	if err := s.BindChildAuthority(t.Context(), nextAuthority, request.Authority.GrantID, childTestTime.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	changed.Action = "discard"
	changed.Authority = nextAuthority
	rebound, err := s.RequestChildDisposition(t.Context(), changed, childTestTime.Add(4*time.Second))
	if err != nil || string(rebound.Plan) != "exact plan" || rebound.PlanDigest != current.PlanDigest {
		t.Fatal("replacement attempt lost exact plan", rebound, err)
	}
	if err := s.CompleteChildDisposition(t.Context(), current, childTestTime.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("old host acknowledged newer owner", err)
	}
	if err := s.CompleteChildDisposition(t.Context(), rebound, childTestTime.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	history, err := s.ChildDispositionHistory(t.Context(), child.Identity)
	if err != nil || len(history) != 2 || history[0].RequestDigest != first.RequestDigest() || history[1].PlanDigest != current.PlanDigest {
		t.Fatal(history, err)
	}
}

func TestChildDispositionHistoryBoundAndFamilyPruning(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	child, request := dispositionFixture(t, s, "parent", childTestTime)
	current, err := s.RequestChildDisposition(t.Context(), request, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	for i := range MaxChildDispositionRevisions {
		request.ExpectedRequestDigest = current.RequestDigest()
		if request.Action == "discard" {
			request.Action = "merge"
		} else {
			request.Action = "discard"
		}
		current, err = s.RequestChildDisposition(t.Context(), request, childTestTime.Add(time.Duration(i+1)*time.Second))
		if err != nil {
			t.Fatal(i, err)
		}
	}
	request.ExpectedRequestDigest = current.RequestDigest()
	request.Action = "replace"
	if _, err := s.RequestChildDisposition(t.Context(), request, childTestTime.Add(time.Minute)); !errors.Is(err, ErrChildDispositionHistoryFull) {
		t.Fatal("history exceeded bound", err)
	}
	if err := s.KeepChildDispositionPlan(t.Context(), current, []byte("plan")); err != nil {
		t.Fatal(err)
	}
	current, err = s.ChildDisposition(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	at := childTestTime.Add(time.Minute)
	if err := s.CompleteChildDisposition(t.Context(), current, at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkChildParentSettled(t.Context(), child.Identity.ChildParent, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneChildren(t.Context(), at.Add(ChildRetention), 100); err != nil {
		t.Fatal(err)
	}
	if n := childTableCount(t, s, "child_disposition_history"); n != 0 {
		t.Fatal("history outlived family", n)
	}
}
