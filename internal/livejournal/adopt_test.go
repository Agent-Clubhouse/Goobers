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

	release, err := w.Adopt("run-refuse", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer release()
	// Reserve must keep refusing an adopted run: another driver holds it.
	if _, ok := w.Reserve("run-refuse"); ok {
		t.Fatal("Reserve took a run that is adopted")
	}

	// A handle whose owner already closed it can never accept an append, and
	// its run-dir lock went with the close. Adopting it would wedge every later
	// emit behind a dead handle instead of letting one reopen the journal.
	closed := runnerHandle(t, runsDir, "run-closed")
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Adopt("run-closed", "web", closed); err == nil || !strings.Contains(err.Error(), "handle is closed") {
		t.Fatalf("Adopt of a closed handle: err = %v", err)
	}
	if w.IsOpen("run-closed") {
		t.Fatal("a refused Adopt registered the closed handle anyway")
	}
}

// TestAdoptRefusesARunThisWriterDrivesItself is ErrAdoptConflict's own case:
// the writer already opened this run's journal, so it is an engine-driven run
// with a driver of its own. Two drivers for one run is the bug, not a case to
// reconcile, so the adoption is refused rather than substituted — and the
// writer's own journal is left exactly as it was.
func TestAdoptRefusesARunThisWriterDrivesItself(t *testing.T) {
	w, _ := testWriter(t)
	if _, err := w.Emit(context.Background(), openBatch("run-driven", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	// The handle the writer opened for itself. Nothing else in the process can
	// hold one for this directory — the run-dir lock is the point — so this is
	// the only way to present a handle for a run the writer already drives.
	w.mu.Lock()
	owned := w.open["run-driven"].jr
	w.mu.Unlock()

	if _, err := w.Adopt("run-driven", "web", owned); !errors.Is(err, ErrAdoptConflict) {
		t.Fatalf("Adopt of a run the writer drives: err = %v, want ErrAdoptConflict", err)
	}
	if !w.IsOpen("run-driven") {
		t.Fatal("the refused Adopt disturbed the writer's own run")
	}
	// Still the writer's: an ordinary emit lands, and CloseIdle still reclaims
	// it (an adoption would have exempted it from both).
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-driven", Gaggle: "web", Ops: []Op{stageArtifactOp("run-driven|0|open-pr|1|0")},
	}); err != nil {
		t.Fatalf("emit after the refusal: %v", err)
	}
	if closed := w.CloseIdle(0); len(closed) != 1 || closed[0] != "run-driven" {
		t.Fatalf("CloseIdle = %v, want the writer's own run reclaimed", closed)
	}
}

// TestAdoptIsRefcountedForConcurrentPodAttempts: decision 003 ruling 5 scopes
// the loan to a pod ATTEMPT, and a run's parallel branches dispatch their
// attempts concurrently (internal/runner/parallel_run.go, one goroutine per
// branch). A per-attempt Adopt/release seam therefore issues overlapping Adopts
// for one run. Two loans of ONE handle are not two writers — they are one
// handle, one lock, taken once — so the second joins the first instead of being
// refused back onto the 30s rehydrate wedge this exists to remove.
func TestAdoptIsRefcountedForConcurrentPodAttempts(t *testing.T) {
	t.Cleanup(journal.SetLockTimeoutForTest(time.Second, 20*time.Millisecond))
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-branches")

	releaseA, err := w.Adopt("run-branches", "web", jr)
	if err != nil {
		t.Fatalf("first Adopt: %v", err)
	}
	releaseB, err := w.Adopt("run-branches", "web", jr)
	if err != nil {
		t.Fatalf("second Adopt of the same handle: %v", err)
	}

	// The first branch finishing must not end the loan the second is still
	// using: an emit after releaseA has to stay on the adopted handle.
	releaseA()
	if !w.IsOpen("run-branches") {
		t.Fatal("releasing one loan ended the other branch's adoption")
	}
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-branches", Gaggle: "web", Ops: []Op{stageArtifactOp("run-branches|2|open-pr|1|0")},
	}); err != nil {
		t.Fatalf("emit on the surviving loan: %v", err)
	}

	releaseB()
	if w.IsOpen("run-branches") {
		t.Fatal("the last release left the adoption registered")
	}
	// The handle is untouched by either release — releasing a loan is not
	// closing a journal.
	if err := jr.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "open-pr", Attempt: 1}); err != nil {
		t.Fatalf("runner handle after both releases: %v", err)
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

// TestAdoptedRunRefusesAnOpAfterITSOWNEROTerminalizes is the terminal latch's
// real shape, and the one the loan changes. On a journal this writer opened,
// the marker applyOp latches on run.finished is complete — it is the only
// appender. On a LOANED one it is not: the runner writes run.finished through
// its own handle (the stalled-run sweep and `goobers run abort` terminalize
// exactly that way, claiming that same handle), so a marker derived at Adopt
// time is a snapshot that never refreshes. Terminality is therefore read off
// the handle, and a straggler pod emit is refused with ErrTerminal instead of
// being appended AFTER the terminal event.
func TestAdoptedRunRefusesAnOpAfterItsOwnerTerminalizes(t *testing.T) {
	t.Cleanup(journal.SetLockTimeoutForTest(time.Second, 20*time.Millisecond))
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-terminalized")
	release, err := w.Adopt("run-terminalized", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer release()

	// An emit BEFORE the terminal lands, so the refusal below cannot be
	// mistaken for the loan never having worked.
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-terminalized", Gaggle: "web", Ops: []Op{stageArtifactOp("run-terminalized|0|open-pr|1|0")},
	}); err != nil {
		t.Fatalf("emit before the terminal: %v", err)
	}

	// The RUNNER terminalizes, through its own handle — never through this
	// plane, so applyOp never sees it.
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatalf("runner terminal append: %v", err)
	}

	straggler := stageArtifactOp("run-terminalized|0|open-pr|1|1")
	resp, err := w.Emit(context.Background(), EmitRequest{RunID: "run-terminalized", Gaggle: "web", Ops: []Op{straggler}})
	if !errors.Is(err, ErrTerminal) {
		t.Fatalf("straggler emit err = %v (resp %+v), want ErrTerminal", err, resp)
	}
	events := readEvents(t, runsDir, "run-terminalized")
	for i, ev := range events {
		if ev.Type == journal.EventRunFinished && i != len(events)-1 {
			t.Fatalf("events after run.finished: %v", eventTypes(events[i+1:]))
		}
	}
}

// TestAdoptedTerminalEventNeverClosesTheOwnersHandle covers the defensive arm:
// a run.finished arriving through THIS plane for an adopted run. The writer
// must latch it without taking the ordinary terminal path, which closes the
// journal, nils the handle and forgets the run — on a handle that belongs to
// the runner. Closing a loaned handle would drop the run-dir lock out from
// under its owner mid-run, which is the one thing Adopt's doc says must never
// happen.
func TestAdoptedTerminalEventNeverClosesTheOwnersHandle(t *testing.T) {
	t.Cleanup(journal.SetLockTimeoutForTest(time.Second, 20*time.Millisecond))
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-terminal-plane")
	release, err := w.Adopt("run-terminal-plane", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer release()

	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	resp, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-terminal-plane", Gaggle: "web",
		Ops: []Op{appendOp("run-terminal-plane|0||0|9", at, journal.Event{
			Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
		})},
	})
	if err != nil {
		t.Fatalf("terminal emit: %v", err)
	}
	if !resp.Terminal {
		t.Fatalf("resp = %+v, want Terminal", resp)
	}
	if !w.IsOpen("run-terminal-plane") {
		t.Fatal("the writer forgot an adopted run on its terminal event")
	}
	// The clinching check: the owner's handle is still open and still holds its
	// lock, so the runner can finish its own bookkeeping.
	if err := jr.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "open-pr", Attempt: 1}); err != nil {
		t.Fatalf("runner handle after the plane's terminal event: %v", err)
	}
	// And the latch holds: a later op is refused, not appended after run.finished.
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-terminal-plane", Gaggle: "web", Ops: []Op{stageArtifactOp("run-terminal-plane|0|open-pr|1|0")},
	}); !errors.Is(err, ErrTerminal) {
		t.Fatalf("post-terminal emit err = %v, want ErrTerminal", err)
	}
	if types := eventTypes(readEvents(t, runsDir, "run-terminal-plane")); types[len(types)-1] != string(journal.EventStageHeartbeat) {
		t.Fatalf("events = %v, want nothing appended by the plane after run.finished", types)
	}
}

// TestAdoptedGateEvaluatedFindsAnArtifactTheOWNERRecorded: the refs an adopted
// run starts with are a snapshot, and the handle's owner keeps recording
// artifacts through it. Once gates are placeable (decision 001) a pod-executed
// gate names a verdict the runner recorded, and refusing it as "unrecorded"
// would be the snapshot talking, not the journal.
func TestAdoptedGateEvaluatedFindsAnArtifactTheOwnerRecorded(t *testing.T) {
	t.Cleanup(journal.SetLockTimeoutForTest(time.Second, 20*time.Millisecond))
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-refs")
	release, err := w.Adopt("run-refs", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer release()

	// Recorded by the RUNNER, after the adoption — this writer never sees it.
	if _, err := jr.RecordArtifact("verdict.json", []byte(`{"outcome":"pass"}`)); err != nil {
		t.Fatalf("runner RecordArtifact: %v", err)
	}
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-refs", Gaggle: "web",
		Ops: []Op{appendOp("run-refs|0|review|1|0", at, journal.Event{
			Type: journal.EventGateEvaluated, Gate: "review", Name: "verdict.json", Verdict: "pass", Target: "ship",
		})},
	}); err != nil {
		t.Fatalf("gate.evaluated naming the runner's artifact: %v", err)
	}
	events := readEvents(t, runsDir, "run-refs")
	last := events[len(events)-1]
	if last.Type != journal.EventGateEvaluated || last.Ref == nil || last.Ref.Digest == "" {
		t.Fatalf("last event = %+v, want gate.evaluated carrying the recorded ref", last)
	}
	// A name nothing recorded is still refused — the refresh is a re-read, not
	// a way past the check.
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-refs", Gaggle: "web",
		Ops: []Op{appendOp("run-refs|0|review|1|1", at, journal.Event{
			Type: journal.EventGateEvaluated, Gate: "review", Name: "absent.json", Verdict: "pass", Target: "ship",
		})},
	}); err == nil || !strings.Contains(err.Error(), "unrecorded artifact") {
		t.Fatalf("gate.evaluated naming nothing: err = %v", err)
	}
}

// TestOwnerClosingTheHandleEndsTheLoanRatherThanWedgingTheRun: a handle the
// owner closes without releasing is dead — every append through it fails — but
// the close also freed the run-dir lock. Holding the loan would wedge the run
// forever behind that dead handle; dropping it lets the next emit reopen the
// journal, which is the ordinary path.
func TestOwnerClosingTheHandleEndsTheLoanRatherThanWedgingTheRun(t *testing.T) {
	t.Cleanup(journal.SetLockTimeoutForTest(time.Second, 20*time.Millisecond))
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-orphan")
	release, err := w.Adopt("run-orphan", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer release()
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	// The emit that discovers the dead handle fails — nothing can be written
	// through it — but it must not leave the adoption in place.
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-orphan", Gaggle: "web", Ops: []Op{stageArtifactOp("run-orphan|0|open-pr|1|0")},
	}); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("emit through the closed handle: err = %v, want journal.ErrClosed", err)
	}
	if w.IsOpen("run-orphan") {
		t.Fatal("the writer kept a loan on a handle its owner had closed")
	}
	// The retry rehydrates and lands, which is the whole point of dropping it.
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-orphan", Gaggle: "web", Ops: []Op{stageArtifactOp("run-orphan|0|open-pr|1|0")},
	}); err != nil {
		t.Fatalf("retry after the loan was dropped: %v", err)
	}
	events := readEvents(t, runsDir, "run-orphan")
	if types := eventTypes(events); types[len(types)-1] != string(journal.EventArtifactRecorded) {
		t.Fatalf("events = %v, want the retried artifact recorded", types)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatalf("seq after the handover: %v", err)
	}
}

// TestReleaseLetsTheOwnerCloseWithoutRacingAnEmit pins release's contract: it
// ends the loan AND leaves nothing able to append through the handle, so the
// owner may close immediately afterwards. An emit arriving later reopens the
// journal for itself — the lock the owner's close released.
func TestReleaseLetsTheOwnerCloseWithoutRacingAnEmit(t *testing.T) {
	t.Cleanup(journal.SetLockTimeoutForTest(time.Second, 20*time.Millisecond))
	w, runsDir := testWriter(t)
	jr := runnerHandle(t, runsDir, "run-released")
	release, err := w.Adopt("run-released", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-released", Gaggle: "web", Ops: []Op{stageArtifactOp("run-released|0|open-pr|1|0")},
	}); err != nil {
		t.Fatalf("emit during the loan: %v", err)
	}
	release()
	release() // idempotent
	if w.IsOpen("run-released") {
		t.Fatal("release left the adoption registered")
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("owner close right after release: %v", err)
	}

	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-released", Gaggle: "web", Ops: []Op{stageArtifactOp("run-released|0|open-pr|1|1")},
	}); err != nil {
		t.Fatalf("emit after release rehydrates: %v", err)
	}
	events := readEvents(t, runsDir, "run-released")
	recorded := 0
	for _, ev := range events {
		if ev.Type == journal.EventArtifactRecorded {
			recorded++
		}
	}
	if recorded != 2 {
		t.Fatalf("artifact.recorded x%d, want the loaned one and the rehydrated one", recorded)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatalf("seq across the handover: %v", err)
	}
}

func eventTypes(events []journal.Event) []string {
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, string(ev.Type))
	}
	return types
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
