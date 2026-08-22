package journal

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fixedClock returns a deterministic, monotonically-advancing clock so tests are
// reproducible (timestamps are excluded from conformance anyway).
func fixedClock() func() time.Time {
	base := time.Date(2026, 7, 13, 5, 0, 0, 0, time.UTC)
	var n int
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Second)
	}
}

func testIdentity() RunIdentity {
	return RunIdentity{
		RunID:           "0af7651916cd43dd8448eb211c80319c",
		Workflow:        "nominate-and-fix",
		WorkflowVersion: 3,
		WorkflowDigest:  Digest([]byte("definition-bytes")),
		GooberDigest:    Digest([]byte("resolved-goobers")),
		Gaggle:          "web",
		Trigger:         Trigger{Kind: TriggerItem, Ref: "issue-8"},
	}
}

func newRun(t *testing.T) (*Run, string) {
	t.Helper()
	root := t.TempDir()
	run, err := Create(root, testIdentity(), map[string][]byte{
		"issue.md": []byte("# Issue 8\nrun journal contract"),
	}, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return run, root
}

func TestObserveActivityRefreshesWatchdogClockWithoutJournalAppend(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	run, root := newRunWithClock(t, func() time.Time { return now })
	t.Cleanup(func() { _ = run.Close() })

	now = now.Add(2 * time.Minute)
	run.ObserveActivity()
	if stale := run.IfLastActivityBefore(now.Add(-time.Minute), func(time.Time) {
		t.Fatal("fresh observed activity was claimed as stale")
	}); stale {
		t.Fatal("observed activity did not refresh the watchdog clock")
	}

	reader, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventRunStarted {
		t.Fatalf("events after observed activity = %+v, want only run.started", events)
	}
}

func newRunWithClock(t *testing.T, clock func() time.Time) (*Run, string) {
	t.Helper()
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(clock))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return run, root
}

func TestCreateAndRoundTrip(t *testing.T) {
	run, root := newRun(t)

	art, err := run.RecordArtifact("plan.txt", []byte("step 1: read spec\nstep 2: build"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	seq := []Event{
		{Type: EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: EventStageFinished, Stage: "implement", Attempt: 1, Status: "success", Ref: &art},
		{Type: EventGateEvaluated, Gate: "review", Verdict: "pass", Target: ""},
		{Type: EventRefTouched, ExternalRef: &ExternalRef{Provider: "github", Kind: "pr", ID: "42", URL: "https://x/42"}},
		{Type: EventRunFinished, Status: string(PhaseCompleted)},
	}
	for _, ev := range seq {
		if err := run.Append(ev); err != nil {
			t.Fatalf("Append %s: %v", ev.Type, err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rd, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}

	// Identity round-trips through run.yaml.
	id, err := rd.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.Workflow != "nominate-and-fix" || id.WorkflowVersion != 3 {
		t.Errorf("identity mismatch: %+v", id)
	}
	if id.WorkflowDigest != testIdentity().WorkflowDigest {
		t.Errorf("workflow digest not pinned: %q", id.WorkflowDigest)
	}
	if len(id.Inputs) != 1 || id.Inputs[0].Name != "issue.md" {
		t.Fatalf("inputs not pinned: %+v", id.Inputs)
	}

	// Events replay in seq order: run.started + artifact.recorded (from
	// RecordArtifact) + 5 appended = 7, contiguous.
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 7 {
		t.Fatalf("want 7 events, got %d", len(events))
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d: seq=%d want %d", i, ev.Seq, i+1)
		}
		if ev.Schema != EventSchema {
			t.Errorf("event %d: schema=%q", i, ev.Schema)
		}
	}
	if events[0].Type != EventRunStarted {
		t.Errorf("first event is %s, want run.started", events[0].Type)
	}
	if events[1].Type != EventArtifactRecorded {
		t.Errorf("second event is %s, want artifact.recorded", events[1].Type)
	}
	if events[len(events)-1].Type != EventRunFinished {
		t.Errorf("last event is %s, want run.finished", events[len(events)-1].Type)
	}

	// Artifact digest verifies on read.
	got, err := rd.ArtifactBytes(art)
	if err != nil {
		t.Fatalf("ArtifactBytes: %v", err)
	}
	if string(got) != "step 1: read spec\nstep 2: build" {
		t.Errorf("artifact content mismatch: %q", got)
	}

	// Input snapshot digest verifies on read.
	if _, err := rd.ArtifactBytes(id.Inputs[0].Ref); err != nil {
		t.Errorf("input digest verify: %v", err)
	}
}

func TestDigestStability(t *testing.T) {
	content := []byte("deterministic artifact bytes")
	want := Digest(content)

	// Same content in two independent runs yields the same digest and dedups to
	// one blob within a run.
	for i := 0; i < 2; i++ {
		root := t.TempDir()
		run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		a, err := run.RecordArtifact("a", content)
		if err != nil {
			t.Fatalf("RecordArtifact a: %v", err)
		}
		b, err := run.RecordArtifact("b", content)
		if err != nil {
			t.Fatalf("RecordArtifact b: %v", err)
		}
		if a.Digest != want || b.Digest != want {
			t.Errorf("digest unstable: a=%s b=%s want=%s", a.Digest, b.Digest, want)
		}
		if a.Path != b.Path {
			t.Errorf("identical content did not dedup: %s vs %s", a.Path, b.Path)
		}
		_ = run.Close()
	}
}

func TestRecordArtifactBoundedChecksScrubbedSize(t *testing.T) {
	root := t.TempDir()
	registry, scrubber := DefaultScrubber()
	run, err := Create(root, testIdentity(), nil, WithScrubber(scrubber), WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	const secret = "secret"
	registry.Register([]byte(secret))
	raw := []byte(strings.Repeat(secret, 171))
	if _, err := run.RecordArtifactBounded("expanded.json", raw, len(raw)); err == nil ||
		!strings.Contains(err.Error(), "after redaction") {
		t.Fatalf("expanded artifact error = %v", err)
	}
	if run.seq != 1 {
		t.Fatalf("rejected artifact advanced journal sequence to %d", run.seq)
	}

	scrubbed := []byte(strings.Repeat(Redacted, 171))
	ref, err := run.RecordArtifactBounded("bounded.json", raw, len(scrubbed))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Size != int64(len(scrubbed)) {
		t.Fatalf("persisted artifact size = %d, limit %d", ref.Size, len(scrubbed))
	}
	data, err := os.ReadFile(filepath.Join(root, testIdentity().RunID, ref.Path))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(scrubbed) {
		t.Fatalf("persisted artifact was not scrubbed: %q", data)
	}
}

func TestRecordBranchArtifactBoundedKeepsAttribution(t *testing.T) {
	run, root := newRun(t)
	t.Cleanup(func() { _ = run.Close() })
	if _, err := run.RecordBranchArtifactBounded(2, "bounded.json", []byte("{}"), 2); err != nil {
		t.Fatal(err)
	}
	rd, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1]; got.Branch != 2 || got.Name != "bounded.json" {
		t.Fatalf("bounded artifact event = %+v, want branch 2 attribution", got)
	}
}

func TestStateCheckpoint(t *testing.T) {
	run, root := newRun(t)
	run.SetMachineState("implement")
	if err := run.Append(Event{Type: EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	_ = run.Close()

	rd, _ := OpenRead(filepath.Join(root, testIdentity().RunID))
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != PhaseRunning {
		t.Errorf("phase=%q want running", st.Phase)
	}
	if st.MachineState != "implement" {
		t.Errorf("machineState=%q want implement", st.MachineState)
	}
	// LastSeq covers run.started + the stage event.
	if st.LastSeq != 2 {
		t.Errorf("lastSeq=%d want 2", st.LastSeq)
	}
}

func TestAppendObserverReceivesEveryDurableSequence(t *testing.T) {
	var observed []uint64
	run, err := Create(t.TempDir(), testIdentity(), nil, WithAppendObserver(
		func(_ string, seq uint64) {
			observed = append(observed, seq)
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	run.SetMachineState("implement")
	if err := run.Append(Event{Type: EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(observed, []uint64{1, 2}) {
		t.Fatalf("observed sequences = %v, want [1 2]", observed)
	}
}

func TestFutureEventSchemaFailsClosed(t *testing.T) {
	run, root := newRun(t)
	_ = run.Close()

	path := filepath.Join(root, testIdentity().RunID, fileEvents)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"schema":"goobers.dev/journal/event/v2","seq":99,"type":"quantum.entangled","branch":0,"time":"2027-01-01T00:00:00Z"}` + "\n")
	_ = f.Close()

	reader, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	_, err = reader.Events()
	if err == nil {
		t.Fatal("Events accepted a newer event schema")
	}
	for _, want := range []string{"event schema", "event/v2", "event/v1", "minimum binary"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Events error %q does not contain %q", err, want)
		}
	}
}
