package triggerqueue

import (
	"path/filepath"
	"testing"
	"time"
)

func TestChildJournalOwnershipPinsEntireFamilyUntilTombstone(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c, _ := failedRestartChild(t, s)
	epoch, _, err := s.BeginChildRestart(t.Context(), childRestartRequest(c, "epoch-1"), childTestTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	runs := []string{c.RunID, c.Identity.ParentRunID, epoch.RunID}
	for _, run := range runs {
		owned, err := s.ChildJournalOwned(t.Context(), c.Identity.Gaggle, run)
		if err != nil || !owned {
			t.Fatal(run, owned, err)
		}
		if other, err := s.ChildJournalOwned(t.Context(), "other", run); err != nil || other {
			t.Fatal("cross-gaggle", other, err)
		}
	}
	if owned, err := s.ChildJournalOwned(t.Context(), c.Identity.Gaggle, "unrelated"); err != nil || owned {
		t.Fatal(owned, err)
	}
	c, err = s.GetChild(t.Context(), c.Identity)
	if err != nil {
		t.Fatal(err)
	}
	now := childTestTime.Add(time.Hour)
	c = settleChildEpoch(t, s, c, now)
	if err = s.MarkChildParentSettled(t.Context(), c.Identity.ChildParent, now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PruneChildren(t.Context(), now.Add(ChildRetention+time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if owned, err := s.ChildJournalOwned(t.Context(), c.Identity.Gaggle, c.RunID); err != nil || !owned {
		t.Fatal("unacknowledged result lost source", owned, err)
	}
	if err = s.AcknowledgeChild(t.Context(), c.Identity, c.ResultRef, now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PruneChildren(t.Context(), now.Add(ChildRetention+time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if owned, err := s.ChildJournalOwned(t.Context(), c.Identity.Gaggle, run); err != nil || owned {
			t.Fatal("tombstone retained journal", run, owned, err)
		}
	}
}
