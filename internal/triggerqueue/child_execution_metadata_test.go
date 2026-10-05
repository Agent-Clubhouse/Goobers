package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestChildExecutionMetadataDoesNotMaterializePlan(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c, _ := failedRestartChild(t, s)
	req := childRestartRequest(c, "human-epoch-1")
	epoch, _, err := s.BeginChildRestart(t.Context(), req, childTestTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately replace the bytes with another bounded payload. Metadata is
	// still inspectable; execution must detect the changed exact plan bytes.
	if _, err := s.db.Exec(`UPDATE child_execution_epochs SET plan=zeroblob(?) WHERE run_id=?`, MaxChildRestartPlanBytes, epoch.RunID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ChildExecutionMetadata(t.Context(), c.Identity, epoch.RunID)
	if err != nil || got.Plan != nil || got.RequestDigest != epoch.RequestDigest || got.PlanDigest != epoch.PlanDigest {
		t.Fatal(got, err)
	}
	history, err := s.ChildExecutionHistory(t.Context(), c.Identity)
	if err != nil || len(history) != 2 || history[0].Plan != nil || history[1].Plan != nil {
		t.Fatal(history, err)
	}
	if _, err := s.ChildExecution(t.Context(), c.Identity, epoch.RunID); !errors.Is(err, ErrConflict) {
		t.Fatal("metadata lookup authorized changed plan", err)
	}
	if _, err := s.db.Exec(`UPDATE child_execution_epochs SET actor='changed' WHERE run_id=?`, epoch.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChildExecutionMetadata(t.Context(), c.Identity, epoch.RunID); !errors.Is(err, ErrConflict) {
		t.Fatal("changed metadata digest", err)
	}
}
