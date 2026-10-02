package readservice

import (
	"testing"
	"time"
)

func TestStartupStatusSnapshotIsDetailedAndIndependent(t *testing.T) {
	started := time.Date(2026, time.September, 12, 19, 30, 0, 0, time.UTC)
	service := &Local{}
	service.AttachStartupStatus(func() *StartupStatus {
		return &StartupStatus{
			Phase:  "worktree-reap-crash-orphan",
			Target: "efunhouse",
			Since:  started,
			BlockingCandidate: &StartupRecoveryCandidate{
				RunID:     "run-1",
				Operation: "resolve execution generation",
			},
		}
	})

	first := service.startupStatusSnapshot()
	if first == nil || first.Phase != "worktree-reap-crash-orphan" ||
		first.Target != "efunhouse" || !first.Since.Equal(started) {
		t.Fatalf("startup status = %+v", first)
	}
	first.Target = "changed"
	first.BlockingCandidate.RunID = "changed"
	if got := service.startupStatusSnapshot(); got.Target != "efunhouse" {
		t.Fatalf("caller mutated shared startup status: %+v", got)
	}
	if got := service.startupStatusSnapshot(); got.BlockingCandidate.RunID != "run-1" {
		t.Fatalf("caller mutated shared startup recovery candidate: %+v", got.BlockingCandidate)
	}
}
