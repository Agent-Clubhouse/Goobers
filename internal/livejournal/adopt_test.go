package livejournal

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// adopt_test.go covers Writer.Adopt's refusals and its lifecycle guards. The
// behaviour at the seam a pod's emit actually crosses — the write API's journal
// plane in front of this writer, against a run the daemon's runner holds open —
// is tested in internal/httpapi/journalplane_adopt_test.go, where both halves
// of that seam are reachable.

// runnerHandle creates a run journal in runsDir and keeps it open, the way
// internal/runner holds a run it is driving (and, with it, the run-dir lock).
func runnerHandle(t *testing.T, runsDir, runID string) *journal.Run {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "wf", WorkflowVersion: 1, WorkflowDigest: "sha256:abc", Gaggle: "web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatalf("create runner-held journal: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	return jr
}

func stageArtifactOp(key string) Op {
	return Op{
		Kind: OpArtifact, Key: key, Time: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
		Artifact: &ArtifactOp{Stage: "open-pr", Attempt: 1, Name: "pr.json", Data: []byte(`{"number":42}`)},
	}
}

func TestAdoptRefusesWhatItCannotSafelyHold(t *testing.T) {
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-refuse")
	other := runnerHandle(t, runsDir, "run-other")

	for name, tc := range map[string]struct {
		runID  string
		gaggle string
		handle *journal.Run
		want   string
	}{
		"invalid run id":      {runID: "../escape", gaggle: "web", handle: jr, want: "invalid run id"},
		"no handle":           {runID: "run-refuse", gaggle: "web", handle: nil, want: "no open journal handle"},
		"unresolvable gaggle": {runID: "run-refuse", gaggle: "nope", handle: jr, want: "no configured runs directory"},
		// A handle open on a DIFFERENT run would land this run's pod bytes in
		// another run's journal — the one mistake adoption could make that the
		// lock would not catch, since that lock is held legitimately.
		"handle on another run": {runID: "run-refuse", gaggle: "web", handle: other, want: "handle is open on"},
	} {
		release, err := w.Adopt(tc.runID, tc.gaggle, tc.handle)
		if err == nil {
			release()
			t.Errorf("%s: Adopt succeeded, want refusal", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
		if w.IsOpen(tc.runID) {
			t.Errorf("%s: a refused Adopt registered the run anyway", name)
		}
	}

	// A second adoption of a run this writer already holds is refused, not
	// silently substituted: two handles on one events.jsonl is exactly what
	// the run-dir lock exists to prevent (D7).
	release, err := w.Adopt("run-refuse", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer release()
	if _, err := w.Adopt("run-refuse", "web", jr); !errors.Is(err, ErrAdoptConflict) {
		t.Fatalf("second Adopt err = %v, want ErrAdoptConflict", err)
	}
	if !w.IsOpen("run-refuse") {
		t.Fatal("the refused second Adopt disturbed the live adoption")
	}
	// Reserve must keep refusing an adopted run: another driver holds it.
	if _, ok := w.Reserve("run-refuse"); ok {
		t.Fatal("Reserve took a run that is adopted")
	}
}

// TestAdoptRefusesARunReservedForRepair: a reservation means the DS5 repairer
// is about to REPLACE the run directory. Emits parked behind it re-derive
// their state afterwards; an adoption cannot, because it caches the applied
// keys the repair is about to rewrite.
func TestAdoptRefusesARunReservedForRepair(t *testing.T) {
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-reserved")
	releaseReservation, ok := w.Reserve("run-reserved")
	if !ok {
		t.Fatal("Reserve refused a run nothing holds")
	}
	if _, err := w.Adopt("run-reserved", "web", jr); err == nil || !strings.Contains(err.Error(), "reserved for repair") {
		t.Fatalf("Adopt during a reservation: err = %v", err)
	}
	releaseReservation()
	release, err := w.Adopt("run-reserved", "web", jr)
	if err != nil {
		t.Fatalf("Adopt after the reservation released: %v", err)
	}
	release()
}

// TestAdoptedRunIsNeverClosedByTheWriter pins the ownership rule: CloseIdle and
// Close release journals this writer OPENED. A loaned handle is neither closed
// (that would take the journal out from under the runner) nor forgotten (that
// would send the next pod emit back down the rehydrate path, into the runner's
// own lock).
func TestAdoptedRunIsNeverClosedByTheWriter(t *testing.T) {
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-loaned")
	release, err := w.Adopt("run-loaned", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer release()

	if closed := w.CloseIdle(0); len(closed) != 0 {
		t.Fatalf("CloseIdle closed %v; an adopted handle is not the writer's to close", closed)
	}
	if !w.IsOpen("run-loaned") {
		t.Fatal("CloseIdle forgot an adopted run")
	}
	w.Close()
	if !w.IsOpen("run-loaned") {
		t.Fatal("Close forgot an adopted run")
	}
	// The clinching check: the runner's handle still works, so neither call
	// closed it.
	if err := jr.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "open-pr", Attempt: 1}); err != nil {
		t.Fatalf("runner handle after CloseIdle/Close: %v", err)
	}
}

// TestAdoptDerivesAppliedKeysFromTheJournal is the daemon-restart shape: the
// runner recovers its handle mid-attempt and re-adopts, and a pod redelivering
// the batch it already emitted must dedupe. The applied keys ride the events
// themselves, so a fresh writer with no memory derives them from the journal.
func TestAdoptDerivesAppliedKeysFromTheJournal(t *testing.T) {
	// An adopted emit never contends the run lock, so this bound is never
	// waited on when the code is right — it is here so a regression fails in a
	// second instead of hanging out the production 30s.
	t.Cleanup(journal.SetLockTimeoutForTest(time.Second, 20*time.Millisecond))
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-restart")
	release, err := w.Adopt("run-restart", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	key := "run-restart|0|open-pr|1|0"
	resp, err := w.Emit(context.Background(), EmitRequest{RunID: "run-restart", Gaggle: "web", Ops: []Op{stageArtifactOp(key)}})
	if err != nil {
		t.Fatalf("emit through the adopted handle: %v", err)
	}
	if resp.Applied != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	release()

	// A new writer — the daemon came back — re-adopts the same handle.
	restarted, err := NewWriter(func(gaggle string) (string, bool) {
		if gaggle != "web" {
			return "", false
		}
		return runsDir, true
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	release, err = restarted.Adopt("run-restart", "web", jr)
	if err != nil {
		t.Fatalf("re-Adopt after restart: %v", err)
	}
	defer release()
	resp, err = restarted.Emit(context.Background(), EmitRequest{RunID: "run-restart", Gaggle: "web", Ops: []Op{stageArtifactOp(key)}})
	if err != nil {
		t.Fatalf("redelivered emit: %v", err)
	}
	if resp.Applied != 0 || resp.Deduplicated != 1 {
		t.Fatalf("redelivered emit = %+v, want it deduplicated from the journal's own keys", resp)
	}

	events := readEvents(t, runsDir, "run-restart")
	recorded := 0
	for _, ev := range events {
		if ev.Type == journal.EventArtifactRecorded {
			recorded++
		}
	}
	if recorded != 1 {
		t.Fatalf("artifact.recorded x%d, want exactly one", recorded)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatalf("one handle, one seq counter: %v", err)
	}
}

// TestAdoptedEmitsAreStampedByTheAdoptedHandlesClock records the deliberate
// consequence of borrowing another driver's handle: journal.Run.Append stamps
// from the clock its handle was built with, so an adopted run's events all
// carry the runner's clock — the op's own Time is not replayed, and cannot be,
// because there is no replayClock to replay it into. A runner-driven run has no
// Temporal history to re-project, so one clock for every event in the journal
// is the coherent reading (see Adopt's doc).
func TestAdoptedEmitsAreStampedByTheAdoptedHandlesClock(t *testing.T) {
	t.Cleanup(journal.SetLockTimeoutForTest(time.Second, 20*time.Millisecond))
	w, runsDir := testWriter(t)
	pinned := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-clocked", Workflow: "wf", WorkflowVersion: 1, WorkflowDigest: "sha256:abc", Gaggle: "web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil, journal.WithClock(func() time.Time { return pinned }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	release, err := w.Adopt("run-clocked", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer release()

	// The op carries a wildly different time (a pod's own wall clock, #3774).
	op := stageArtifactOp("run-clocked|0|open-pr|1|0")
	if _, err := w.Emit(context.Background(), EmitRequest{RunID: "run-clocked", Gaggle: "web", Ops: []Op{op}}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	events := readEvents(t, runsDir, "run-clocked")
	last := events[len(events)-1]
	if last.Type != journal.EventArtifactRecorded {
		t.Fatalf("last event = %s", last.Type)
	}
	if !last.Time.Equal(pinned) {
		t.Fatalf("artifact.recorded time = %s, want the adopted handle's clock %s", last.Time, pinned)
	}
}

// TestUnadoptedRunStillOpensItsOwnJournal guards against the adoption path
// leaking into the ordinary one: a run nobody adopted is opened, closed and
// forgotten by this writer exactly as before.
func TestUnadoptedRunStillOpensItsOwnJournal(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-owned", started)); err != nil {
		t.Fatal(err)
	}
	if !w.IsOpen("run-owned") {
		t.Fatal("the writer should hold a run it opened")
	}
	if closed := w.CloseIdle(0); len(closed) != 1 || closed[0] != "run-owned" {
		t.Fatalf("CloseIdle = %v, want the writer's own run released", closed)
	}
	if w.IsOpen("run-owned") {
		t.Fatal("the writer's own idle run should be forgotten")
	}
	// The lock is free again, which is CloseIdle's whole purpose.
	restore := journal.SetLockTimeoutForTest(500*time.Millisecond, 10*time.Millisecond)
	jr, _, err := journal.Recover(filepath.Join(runsDir, "run-owned"))
	restore()
	if err != nil {
		t.Fatalf("Recover after CloseIdle: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
}
