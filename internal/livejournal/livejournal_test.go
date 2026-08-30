package livejournal

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func testWriter(t *testing.T, opts ...Option) (*Writer, string) {
	t.Helper()
	runsDir := filepath.Join(t.TempDir(), "runs")
	w, err := NewWriter(func(gaggle string) (string, bool) {
		if gaggle != "web" {
			return "", false
		}
		return runsDir, true
	}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w, runsDir
}

func openHeader(runID string) *OpenHeader {
	return &OpenHeader{
		Identity: journal.RunIdentity{
			RunID: runID, Workflow: "wf", WorkflowVersion: 1, WorkflowDigest: "sha256:abc", Gaggle: "web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		},
		Graph:      []byte(`{"nodes":[]}`),
		Definition: []byte(`{"name":"wf"}`),
	}
}

func appendOp(key string, at time.Time, ev journal.Event) Op {
	e := ev
	return Op{Kind: OpAppend, Key: key, Event: &e, Time: at}
}

func openBatch(runID string, at time.Time) EmitRequest {
	return EmitRequest{
		RunID:  runID,
		Gaggle: "web",
		Open:   openHeader(runID),
		Ops: []Op{
			appendOp(runID+"|0||0|0", at, journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)}),
			appendOp(runID+"|0|build|1|0", at.Add(time.Second), journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}),
		},
	}
}

func readEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestEmitCreatesJournalAtFirstEmitAndStampsOpTimes(t *testing.T) {
	var observed []uint64
	w, runsDir := testWriter(t, WithObserver(func(runID string, seq uint64) { observed = append(observed, seq) }))
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	resp, err := w.Emit(context.Background(), openBatch("run-live", started))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if resp.Applied != 1 || resp.Deduplicated != 1 || resp.Seq != 2 || resp.Terminal {
		// run.started is satisfied by journal creation (deduplicated), the
		// stage.started append is the applied op.
		t.Fatalf("resp = %+v", resp)
	}
	events := readEvents(t, runsDir, "run-live")
	if len(events) != 2 || events[0].Type != journal.EventRunStarted || events[1].Type != journal.EventStageStarted {
		t.Fatalf("events = %+v", events)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatal(err)
	}
	if !events[0].Time.Equal(started) || !events[1].Time.Equal(started.Add(time.Second)) {
		t.Fatalf("event times not replayed from ops: %v, %v", events[0].Time, events[1].Time)
	}
	if key, _ := events[1].Runner[EmitKeyRunnerField].(string); key != "run-live|0|build|1|0" {
		t.Fatalf("stage.started emit key = %q", key)
	}
	if len(observed) == 0 {
		t.Fatal("append observer never notified")
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-live"))
	if err != nil {
		t.Fatal(err)
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatal(err)
	}
	if phase != journal.PhaseRunning {
		t.Fatalf("phase = %s, want running", phase)
	}
	if !w.IsOpen("run-live") {
		t.Fatal("writer should hold the run open")
	}
}

// TestApplyOpRefusesToZeroTheClockOnAnUnstampedOp is #3774 defense in depth
// at the exact seam applyOp crosses (run.clock.set(op.Time), unconditional
// before this fix): a pod-side writer defect (fixed at its two call sites,
// cmd/goobers's podArtifactRecorder.Append and recordStageArtifacts) could
// still reach here for some future emitter that forgets to stamp a Time. A
// zero Time reaching applyOp must not zero the run's clock and, through it,
// the very event this op is about to append — the clock instead holds its
// last known-good value, so the affected event lands with a stale-but-real
// timestamp rather than 0001-01-01T00:00:00Z.
func TestApplyOpRefusesToZeroTheClockOnAnUnstampedOp(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := w.Emit(context.Background(), openBatch("run-clock", started)); err != nil {
		t.Fatalf("open batch: %v", err)
	}
	lastGood := started.Add(time.Second) // openBatch's stage.started op's stamped Time

	// The exact shape #3774's writer defect produced: an Op with no Time set.
	unstamped := journal.Event{Type: journal.EventStageFinished, Stage: "build", Attempt: 1, Status: "success"}
	if _, err := w.Emit(context.Background(), EmitRequest{RunID: "run-clock", Gaggle: "web", Ops: []Op{
		{Kind: OpAppend, Key: "run-clock|0|build|1|1", Event: &unstamped},
	}}); err != nil {
		t.Fatalf("unstamped emit: %v", err)
	}

	events := readEvents(t, runsDir, "run-clock")
	if len(events) != 3 {
		t.Fatalf("events = %+v, want 3", events)
	}
	got := events[2]
	if got.Type != journal.EventStageFinished {
		t.Fatalf("events[2] = %+v, want stage.finished", got)
	}
	if got.Time.IsZero() {
		t.Fatal("unstamped op's event landed with a zero Time (#3774): replayClock.set must refuse to adopt a zero Time rather than zeroing the clock")
	}
	if !got.Time.Equal(lastGood) {
		t.Fatalf("unstamped op's event.Time = %s, want the clock's last known-good time %s (held, not zeroed)", got.Time, lastGood)
	}
}

func TestEmitDeduplicatesRetriedBatch(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Now().UTC().Truncate(time.Second)
	batch := openBatch("run-retry", started)
	if _, err := w.Emit(context.Background(), batch); err != nil {
		t.Fatalf("first emit: %v", err)
	}
	resp, err := w.Emit(context.Background(), batch)
	if err != nil {
		t.Fatalf("retried emit: %v", err)
	}
	if resp.Applied != 0 || resp.Deduplicated != 2 {
		t.Fatalf("retried resp = %+v, want all deduplicated", resp)
	}
	events := readEvents(t, runsDir, "run-retry")
	if len(events) != 2 {
		t.Fatalf("journal has %d events after redelivery, want exactly 2 (one copy per key)", len(events))
	}
}

func TestEmitDedupSurvivesWriterRestart(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Now().UTC().Truncate(time.Second)
	batch := openBatch("run-restart", started)
	if _, err := w.Emit(context.Background(), batch); err != nil {
		t.Fatalf("emit: %v", err)
	}
	w.Close()

	// A fresh writer (daemon restart) must derive dedup state from the
	// journal itself.
	restarted, err := NewWriter(func(string) (string, bool) { return runsDir, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	redelivered := batch
	redelivered.Ops = append(redelivered.Ops,
		appendOp("run-restart|0|build|1|1", started.Add(2*time.Second), journal.Event{
			Type: journal.EventStageFinished, Stage: "build", Attempt: 1, Status: string(apiv1.ResultSuccess),
		}))
	resp, err := restarted.Emit(context.Background(), redelivered)
	if err != nil {
		t.Fatalf("emit after restart: %v", err)
	}
	if resp.Applied != 1 || resp.Deduplicated != 2 {
		t.Fatalf("resp after restart = %+v", resp)
	}
	events := readEvents(t, runsDir, "run-restart")
	if len(events) != 3 || events[2].Type != journal.EventStageFinished || events[2].Seq != 3 {
		t.Fatalf("events after restart = %+v", events)
	}
}

func TestEmitTerminalClosesJournalAndRefusesNewOps(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-done", started)); err != nil {
		t.Fatal(err)
	}
	terminal := EmitRequest{RunID: "run-done", Gaggle: "web", Ops: []Op{
		appendOp("run-done|0||0|1", started.Add(time.Minute), journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}),
	}}
	resp, err := w.Emit(context.Background(), terminal)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Terminal {
		t.Fatalf("resp = %+v, want terminal", resp)
	}
	if w.IsOpen("run-done") {
		t.Fatal("terminal run should be closed and forgotten")
	}

	// A straggling redelivery of the terminal batch is a no-op.
	again, err := w.Emit(context.Background(), terminal)
	if err != nil {
		t.Fatalf("redelivered terminal batch: %v", err)
	}
	if again.Applied != 0 || again.Deduplicated != 1 || !again.Terminal {
		t.Fatalf("redelivered terminal resp = %+v", again)
	}

	// A genuinely new op after the terminal is refused.
	_, err = w.Emit(context.Background(), EmitRequest{RunID: "run-done", Gaggle: "web", Ops: []Op{
		appendOp("run-done|0|late|1|0", started.Add(2*time.Minute), journal.Event{Type: journal.EventStageStarted, Stage: "late", Attempt: 1}),
	}})
	if !errors.Is(err, ErrTerminal) {
		t.Fatalf("post-terminal emit error = %v, want ErrTerminal", err)
	}
	events := readEvents(t, runsDir, "run-done")
	if events[len(events)-1].Type != journal.EventRunFinished {
		t.Fatalf("terminal journal ends with %s", events[len(events)-1].Type)
	}
}

func TestEmitArtifactWiresGateVerdictRefAcrossRestart(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-gate", started)); err != nil {
		t.Fatal(err)
	}
	verdict := []byte(`{"decision":"pass"}`)
	if _, err := w.Emit(context.Background(), EmitRequest{RunID: "run-gate", Gaggle: "web", Ops: []Op{
		{Kind: OpArtifact, Key: "run-gate|0||0|1", Time: started.Add(2 * time.Second), Artifact: &ArtifactOp{
			Name: "verdict/review-1.json", Data: verdict, Integrity: apiv1.IntegrityDerived,
		}},
	}}); err != nil {
		t.Fatal(err)
	}

	// Restart between the artifact and the gate.evaluated referencing it: the
	// ref table must rehydrate from the journal.
	w.Close()
	restarted, err := NewWriter(func(string) (string, bool) { return runsDir, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if _, err := restarted.Emit(context.Background(), EmitRequest{RunID: "run-gate", Gaggle: "web", Ops: []Op{
		appendOp("run-gate|0|review|0|0", started.Add(3*time.Second), journal.Event{
			Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Name: "verdict/review-1.json", Integrity: apiv1.IntegrityDerived,
		}),
	}}); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, runsDir, "run-gate")
	last := events[len(events)-1]
	if last.Type != journal.EventGateEvaluated || last.Ref == nil || last.Ref.Digest == "" {
		t.Fatalf("gate.evaluated not wired to verdict artifact: %+v", last)
	}
	if artifact := events[len(events)-2]; artifact.Ref == nil || artifact.Ref.Digest != last.Ref.Digest {
		t.Fatalf("gate ref %q does not match recorded artifact", last.Ref.Digest)
	}
}

type fakeSpans struct {
	blobs map[string][]byte
}

func (f *fakeSpans) Get(_ context.Context, digest string) ([]byte, error) {
	data, ok := f.blobs[digest]
	if !ok {
		return nil, errors.New("no such digest")
	}
	return data, nil
}

func TestSpanOpsAdoptOrDegradeToUnavailable(t *testing.T) {
	transcript := []byte("harness transcript")
	digest := journal.Digest(transcript)
	w, runsDir := testWriter(t, WithSpanSource(&fakeSpans{blobs: map[string][]byte{digest: transcript}}))
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-span", started)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Emit(context.Background(), EmitRequest{RunID: "run-span", Gaggle: "web", Ops: []Op{
		{Kind: OpSpan, Key: "run-span|0|build|1|1", Time: started.Add(2 * time.Second), Span: &SpanOp{
			Stage: "build", Attempt: 1, Name: "build.transcript", Ref: journal.Ref{Digest: digest},
		}},
		{Kind: OpSpan, Key: "run-span|0|build|1|2", Time: started.Add(3 * time.Second), Span: &SpanOp{
			Stage: "build", Attempt: 1, Name: "build.missing", Ref: journal.Ref{Digest: "sha256:missing"},
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, runsDir, "run-span")
	adopted, unavailable := events[len(events)-2], events[len(events)-1]
	if adopted.Type != journal.EventSpanRecorded || adopted.Ref == nil || adopted.Ref.Digest != digest {
		t.Fatalf("span not adopted: %+v", adopted)
	}
	if unavailable.Type != journal.EventError || unavailable.Error == nil || unavailable.Error.Code != SpanUnavailableErrorCode {
		t.Fatalf("missing span not degraded to %s: %+v", SpanUnavailableErrorCode, unavailable)
	}
	if key, _ := unavailable.Runner[EmitKeyRunnerField].(string); key == "" {
		t.Fatal("unavailable-span error event carries no emit key; a redelivery would duplicate it")
	}
}

func TestCloseIdleReleasesTheRunLockAndEmitReopens(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-idle", started)); err != nil {
		t.Fatal(err)
	}
	closed := w.CloseIdle(0)
	if len(closed) != 1 || closed[0] != "run-idle" {
		t.Fatalf("CloseIdle = %v", closed)
	}
	if w.IsOpen("run-idle") {
		t.Fatal("idle run should be forgotten")
	}
	// The run-dir lock is free: another owner can recover the journal without
	// waiting out the lock timeout.
	restore := journal.SetLockTimeoutForTest(500*time.Millisecond, 10*time.Millisecond)
	jr, _, err := journal.Recover(filepath.Join(runsDir, "run-idle"))
	restore()
	if err != nil {
		t.Fatalf("Recover after CloseIdle: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	// A later emission transparently rehydrates.
	resp, err := w.Emit(context.Background(), EmitRequest{RunID: "run-idle", Gaggle: "web", Ops: []Op{
		appendOp("run-idle|0|build|1|1", started.Add(time.Minute), journal.Event{
			Type: journal.EventStageFinished, Stage: "build", Attempt: 1, Status: string(apiv1.ResultSuccess),
		}),
	}})
	if err != nil {
		t.Fatalf("emit after idle close: %v", err)
	}
	if resp.Applied != 1 {
		t.Fatalf("resp = %+v", resp)
	}
}

// TestEmitBlockedBehindTerminalizerIsRefusedNotAppended is the #3529 blocker's
// probe shape: CloseIdle released the run-dir lock (its whole purpose — so the
// stalled-run sweep or `goobers run abort` can terminalize a quiet run), a
// terminalizer takes that lock and appends run.finished, and a late Emit that
// was parked on the lock the whole time must derive its state from its own
// under-lock read: the new op is refused with ErrTerminal (the 409
// journal_terminal surface), never appended after the terminal event on the
// strength of state read before the lock.
func TestEmitBlockedBehindTerminalizerIsRefusedNotAppended(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-race", started)); err != nil {
		t.Fatal(err)
	}
	if closed := w.CloseIdle(0); len(closed) != 1 {
		t.Fatalf("CloseIdle = %v", closed)
	}

	// The terminalizer takes — and holds — the run-dir lock, run.finished not
	// yet appended: exactly the window the pre-read raced.
	jr, _, err := journal.Recover(filepath.Join(runsDir, "run-race"))
	if err != nil {
		t.Fatalf("terminalizer Recover: %v", err)
	}

	type result struct {
		resp EmitResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, emitErr := w.Emit(context.Background(), EmitRequest{RunID: "run-race", Gaggle: "web", Ops: []Op{
			appendOp("run-race|0|build|1|1", started.Add(time.Minute), journal.Event{
				Type: journal.EventStageFinished, Stage: "build", Attempt: 1, Status: string(apiv1.ResultSuccess),
			}),
		}})
		done <- result{resp, emitErr}
	}()

	// Let the emit read whatever it reads without the lock and park on the
	// lock; then terminalize and release. With the under-lock derivation the
	// outcome is interleaving-independent — however much of this pause the
	// emit used, it acquires the writer only after run.finished is durable
	// and must see it.
	time.Sleep(300 * time.Millisecond)
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted)}); err != nil {
		t.Fatalf("terminalize: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	res := <-done
	if !errors.Is(res.err, ErrTerminal) {
		t.Fatalf("late emit = (%+v, %v), want ErrTerminal", res.resp, res.err)
	}
	events := readEvents(t, runsDir, "run-race")
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatal(err)
	}
	if last := events[len(events)-1]; last.Type != journal.EventRunFinished {
		t.Fatalf("journal ends with %s — an op was appended after run.finished", last.Type)
	}
	for _, ev := range events {
		if ev.Type == journal.EventStageFinished {
			t.Fatalf("refused op landed in the journal anyway: %+v", ev)
		}
	}
}

func TestEmitRefusesUnknownRunWithoutOpenHeader(t *testing.T) {
	w, _ := testWriter(t)
	_, err := w.Emit(context.Background(), EmitRequest{RunID: "run-none", Gaggle: "web", Ops: []Op{
		appendOp("run-none|0||0|0", time.Now(), journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)}),
	}})
	if !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("err = %v, want ErrUnknownRun", err)
	}
}

func TestEmitRefusesUnresolvableGaggleAndKeylessOps(t *testing.T) {
	w, _ := testWriter(t)
	if _, err := w.Emit(context.Background(), EmitRequest{RunID: "run-x", Gaggle: "nope", Ops: []Op{
		appendOp("k", time.Now(), journal.Event{Type: journal.EventRunStarted}),
	}}); err == nil || !strings.Contains(err.Error(), "no configured runs directory") {
		t.Fatalf("unresolvable gaggle err = %v", err)
	}
	if _, err := w.Emit(context.Background(), EmitRequest{RunID: "run-x", Gaggle: "web", Ops: []Op{
		{Kind: OpAppend, Event: &journal.Event{Type: journal.EventRunStarted}},
	}}); err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("keyless op err = %v", err)
	}
}

// TestReserveParksEmitsAndRefusesOpenRuns pins the repairer coordination
// surface (#3529): Reserve is refused while the writer holds the run open
// (the atomic form of the reconciler's old IsOpen peek), only one reservation
// exists at a time, and an Emit arriving during a reservation parks until
// release instead of rehydrating a journal the repairer may replace.
func TestReserveParksEmitsAndRefusesOpenRuns(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-res", started)); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.Reserve("run-res"); ok {
		t.Fatal("Reserve granted while the writer holds the run open")
	}
	if closed := w.CloseIdle(0); len(closed) != 1 {
		t.Fatalf("CloseIdle = %v", closed)
	}
	release, ok := w.Reserve("run-res")
	if !ok {
		t.Fatal("Reserve refused for a closed run")
	}
	if _, again := w.Reserve("run-res"); again {
		t.Fatal("second concurrent reservation granted")
	}

	done := make(chan error, 1)
	go func() {
		_, err := w.Emit(context.Background(), EmitRequest{RunID: "run-res", Gaggle: "web", Ops: []Op{
			appendOp("run-res|0|build|1|1", started.Add(time.Minute), journal.Event{
				Type: journal.EventStageFinished, Stage: "build", Attempt: 1, Status: string(apiv1.ResultSuccess),
			}),
		}})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("emit completed during the reservation (err=%v), want it parked", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("parked emit after release: %v", err)
	}
	events := readEvents(t, runsDir, "run-res")
	if last := events[len(events)-1]; last.Type != journal.EventStageFinished {
		t.Fatalf("journal ends with %s, want the released emit's stage.finished", last.Type)
	}
}

func TestAuthoredDetectsLiveJournals(t *testing.T) {
	w, runsDir := testWriter(t)
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := w.Emit(context.Background(), openBatch("run-auth", started)); err != nil {
		t.Fatal(err)
	}
	if !Authored(readEvents(t, runsDir, "run-auth")) {
		t.Fatal("live journal not detected as authored")
	}
	if Authored([]journal.Event{{Type: journal.EventRunStarted}}) {
		t.Fatal("keyless events must not read as live-authored")
	}
}
