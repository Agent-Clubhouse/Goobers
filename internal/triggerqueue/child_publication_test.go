package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestChildPublicationIntentReopenConflictAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s := openTestStore(t, path)
	c := acceptChildTest(t, s, childRequest("parent", "occurrence", "key"), childTestTime)
	intent, err := s.PrepareChildPublication(t.Context(), c.Identity, "branch", []byte(`{"sha":"desired"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmChildPublication(t.Context(), intent, []byte(`{"sha":"desired"}`)); !errors.Is(err, ErrTransition) {
		t.Fatal("confirmation without effect admission", err)
	}
	if err = s.BeginChildPublicationEffect(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	replay, err := s.ChildPublication(t.Context(), c.Identity, "branch")
	if err != nil || replay.State != "effect_pending" || replay.Digest != intent.Digest {
		t.Fatal(replay, err)
	}
	if _, err = s.PrepareChildPublication(t.Context(), c.Identity, "branch", []byte(`{"sha":"changed"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = s.FenceChildParent(t.Context(), c.Identity.ChildParent, "operator", childTestTime); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginChildPublicationEffect(t.Context(), intent); !errors.Is(err, ErrParentCancelled) {
		t.Fatal("cancelled effect restarted", err)
	}
	if _, err = s.PrepareChildPublication(t.Context(), c.Identity, "pr", []byte(`{"title":"PR"}`)); !errors.Is(err, ErrParentCancelled) {
		t.Fatal(err)
	}
	receipt := []byte(`{"sha":"desired","confirmed":true}`)
	if err = s.ConfirmChildPublication(t.Context(), intent, receipt); err != nil {
		t.Fatal("observed effect lost on cancellation", err)
	}
	if err = s.ConfirmChildPublication(t.Context(), intent, receipt); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmChildPublication(t.Context(), intent, []byte(`{"sha":"changed"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE child_publications SET receipt='{"sha":"tampered"}' WHERE child_id=?`, c.ChildID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ChildPublication(t.Context(), c.Identity, "branch"); !errors.Is(err, ErrChildPublicationUnavailable) {
		t.Fatal("tampered receipt accepted", err)
	}
}

func TestChildPublicationBoundsAndProductionLineagePruner(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	c := acceptChildTest(t, s, childRequest("parent", "occurrence", "key"), childTestTime)
	for _, action := range []string{"branch", "pr"} {
		p, err := s.PrepareChildPublication(t.Context(), c.Identity, action, []byte(`{"intent":true}`))
		if err != nil {
			t.Fatal(err)
		}
		if err = s.BeginChildPublicationEffect(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		if err = s.ConfirmChildPublication(t.Context(), p, []byte(`{"observed":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PrepareChildPublication(t.Context(), c.Identity, "third", []byte(`{}`)); !errors.Is(err, ErrChildPublicationUnavailable) {
		t.Fatal(err)
	}
	tooLarge := make([]byte, MaxChildPublicationIntentBytes+1)
	if _, err := s.PrepareChildPublication(t.Context(), c.Identity, "branch", tooLarge); !errors.Is(err, ErrChildPublicationUnavailable) {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: "owned-result"}, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeChild(t.Context(), c.Identity, "owned-result", childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkChildParentSettled(t.Context(), c.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	result, err := s.PruneChildren(t.Context(), childTestTime.Add(ChildRetention+time.Hour), 1)
	if err != nil || result.Tombstoned != 1 {
		t.Fatal(result, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM child_publications`).Scan(&count); err != nil || count != 0 {
		t.Fatal("publication rows escaped lineage pruner", count, err)
	}
}

func TestChildPublicationMissingAdmittedRecordCannotBeRecreated(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	c := acceptChildTest(t, s, childRequest("parent", "occurrence", "key"), childTestTime)
	for _, action := range []string{"branch", "pr"} {
		intent := []byte(`{"intent":true}`)
		if _, err := s.PrepareChildPublication(t.Context(), c.Identity, action, intent); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`DELETE FROM child_publications WHERE child_id=? AND action=?`, c.ChildID, action); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PrepareChildPublication(t.Context(), c.Identity, action, intent); !errors.Is(err, ErrChildPublicationUnavailable) {
			t.Fatal("lost intent was recreated", err)
		}
	}
}

func TestChildPublicationPendingPinsTerminalCancelledFamilyUntilObservation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := acceptChildTest(t, s, childRequest("parent", "occurrence", "key"), childTestTime)
	intent, err := s.PrepareChildPublication(t.Context(), c.Identity, "pr", []byte(`{"head":"exact"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginChildPublicationEffect(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginChildPublicationEffect(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if err = s.FenceChildParent(t.Context(), c.Identity.ChildParent, "operator", childTestTime); err != nil {
		t.Fatal(err)
	}
	failChildTest(t, s, c, childTestTime, true)
	if err = s.MarkChildParentSettled(t.Context(), c.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	after := childTestTime.Add(3 * ChildRetention)
	if pruned, err := s.PruneChildren(t.Context(), after, 100); err != nil || pruned.Tombstoned != 0 {
		t.Fatal("uncertain effect pruned", pruned, err)
	}
	if err = s.BeginChildPublicationEffect(t.Context(), intent); !errors.Is(err, ErrParentCancelled) {
		t.Fatal("new effect allowed after cancellation", err)
	}
	if err = s.ConfirmChildPublication(t.Context(), intent, []byte(`{"id":"7","observed":true}`)); err != nil {
		t.Fatal("late observation refused", err)
	}
	child, err := s.GetChild(t.Context(), c.Identity)
	if err != nil || child.ResultRef != "result:"+c.ChildID || child.State != ChildFailed {
		t.Fatal("late observation changed immutable result", child, err)
	}
	if pruned, err := s.PruneChildren(t.Context(), after, 100); err != nil || pruned.Tombstoned != 1 {
		t.Fatal("settled effect stayed pinned", pruned, err)
	}
}
