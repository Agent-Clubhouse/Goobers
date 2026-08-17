package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runPhaseFixture builds one run journal shape and names it, so every test here
// asserts against the same corpus and a new shape is added in one place.
type runPhaseFixture struct {
	name string
	// build appends the events that give the run its shape. The run is created
	// and closed by the harness.
	build func(t *testing.T, run *Run)
	// mangle optionally corrupts the finished journal on disk (torn tails,
	// crash fill) — the shapes a full read tolerates and a bounded scan must
	// tolerate identically.
	mangle func(t *testing.T, dir string)
	want   RunPhase
}

func runPhaseFixtures() []runPhaseFixture {
	finish := func(status RunPhase) func(*testing.T, *Run) {
		return func(t *testing.T, run *Run) {
			t.Helper()
			mustAppend(t, run, Event{Type: EventRunFinished, Status: string(status)})
		}
	}
	return []runPhaseFixture{
		{
			name:  "no events beyond creation",
			build: func(*testing.T, *Run) {},
			want:  PhaseRunning,
		},
		{
			name: "started but never finished",
			build: func(t *testing.T, run *Run) {
				mustAppend(t, run, Event{Type: EventStageStarted, Target: "implement"})
			},
			want: PhaseRunning,
		},
		{name: "finished completed", build: finish(PhaseCompleted), want: PhaseCompleted},
		{name: "finished failed", build: finish(PhaseFailed), want: PhaseFailed},
		{name: "finished aborted", build: finish(PhaseAborted), want: PhaseAborted},
		{name: "finished escalated", build: finish(PhaseEscalated), want: PhaseEscalated},
		{
			// The resurrection shape reconstructPhase exists for: a terminal run
			// reopened by an operator is running again, and a scan that stopped at
			// the first run.finished it met would miss it.
			name: "finished then resumed",
			build: func(t *testing.T, run *Run) {
				mustAppend(t, run, Event{Type: EventRunFinished, Status: string(PhaseFailed)})
				mustAppend(t, run, Event{Type: EventRunResumed, Target: "implement"})
			},
			want: PhaseRunning,
		},
		{
			name: "finished then stage rerun requested",
			build: func(t *testing.T, run *Run) {
				mustAppend(t, run, Event{Type: EventRunFinished, Status: string(PhaseCompleted)})
				mustAppend(t, run, Event{Type: EventStageRerunRequested, Target: "implement"})
			},
			want: PhaseRunning,
		},
		{
			name: "finished then gate overridden",
			build: func(t *testing.T, run *Run) {
				mustAppend(t, run, Event{Type: EventRunFinished, Status: string(PhaseFailed)})
				mustAppend(t, run, Event{Type: EventGateOverridden, Target: "review"})
			},
			want: PhaseRunning,
		},
		{
			name: "resumed then finished again",
			build: func(t *testing.T, run *Run) {
				mustAppend(t, run, Event{Type: EventRunFinished, Status: string(PhaseFailed)})
				mustAppend(t, run, Event{Type: EventRunResumed, Target: "implement"})
				mustAppend(t, run, Event{Type: EventRunFinished, Status: string(PhaseCompleted)})
			},
			want: PhaseCompleted,
		},
		{
			// The decisive record sits far behind the tail, so the scan has to
			// step backwards through several chunks to reach it. Without the
			// geometric backward walk this is the shape that reads as running.
			name:   "decisive record buried under many later records",
			build:  finish(PhaseAborted),
			mangle: padJournal(400, 1024),
			want:   PhaseAborted,
		},
		{
			// A record larger than one backward step: the window must grow past
			// it rather than mistake its interior for a record boundary.
			name: "decisive record behind one oversized record",
			build: func(t *testing.T, run *Run) {
				mustAppend(t, run, Event{Type: EventRunFinished, Status: string(PhaseEscalated)})
				mustAppend(t, run, Event{Type: EventStageFinished, Target: strings.Repeat("x", 200*1024)})
			},
			want: PhaseEscalated,
		},
		{
			// A payload that quotes a decisive type name must not decide the
			// phase — only the record's own type does.
			name: "later record quotes a decisive type in its payload",
			build: func(t *testing.T, run *Run) {
				mustAppend(t, run, Event{Type: EventRunFinished, Status: string(PhaseFailed)})
				mustAppend(t, run, Event{Type: EventStageRerunRequested, Target: "implement"})
				mustAppend(t, run, Event{Type: EventStageFinished, Target: "implement",
					Runner: map[string]any{"note": `saw "run.finished" in the log`}})
			},
			want: PhaseRunning,
		},
		{
			// An interrupted append leaves bytes after the last newline. They are
			// not a durable record and must not be read as one.
			name:  "torn partial final record",
			build: finish(PhaseCompleted),
			mangle: func(t *testing.T, dir string) {
				appendRawToEvents(t, dir, []byte(`{"type":"run.resumed"`))
			},
			want: PhaseCompleted,
		},
		{
			// #116's cascade: crash zero-fill that a later append ran past, so the
			// journal carries a newline-terminated fill-only line after the last
			// valid record. Scanning must walk through it, not stop at it.
			name:  "NUL fill cascade after the decisive record",
			build: finish(PhaseAborted),
			mangle: func(t *testing.T, dir string) {
				appendRawToEvents(t, dir, append(bytes.Repeat([]byte{0}, 4096), '\n'))
			},
			want: PhaseAborted,
		},
		{
			name:  "NUL fill cascade with no decisive record at all",
			build: func(*testing.T, *Run) {},
			mangle: func(t *testing.T, dir string) {
				appendRawToEvents(t, dir, append(bytes.Repeat([]byte{0}, 4096), '\n'))
			},
			want: PhaseRunning,
		},
	}
}

// TestPhaseBoundedAgreesWithPhase is the equivalence #2755's fix rests on:
// reading a bounded tail is only safe because it answers what the full read
// answers. Every shape is asserted against Phase itself, not against a
// hand-written expectation alone, so a future change to reconstructPhase that
// the bounded scan does not learn about fails here rather than silently
// mis-counting active runs at daemon boot.
func TestPhaseBoundedAgreesWithPhase(t *testing.T) {
	for _, tc := range runPhaseFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			dir := buildRunPhaseFixture(t, tc)
			rd, err := OpenRead(dir)
			if err != nil {
				t.Fatalf("OpenRead: %v", err)
			}

			full, err := rd.Phase()
			if err != nil {
				t.Fatalf("Phase: %v", err)
			}
			if full != tc.want {
				t.Fatalf("Phase = %q, want %q (the fixture, not the fix, is wrong)", full, tc.want)
			}
			bounded, err := rd.PhaseBounded(context.Background())
			if err != nil {
				t.Fatalf("PhaseBounded: %v", err)
			}
			if bounded != full {
				t.Errorf("PhaseBounded = %q, Phase = %q", bounded, full)
			}
		})
	}
}

// TestPhaseBoundedFallsBackWhenBudgetIsExhausted pins the safety valve: a
// journal whose decisive record sits beyond the byte budget must produce the
// right phase anyway, by falling back to the full read. Guessing here — or
// defaulting to running — would mis-seed the daemon's concurrency caps, which
// is worse than being slow on the pathological run.
func TestPhaseBoundedFallsBackWhenBudgetIsExhausted(t *testing.T) {
	dir := buildRunPhaseFixture(t, runPhaseFixture{
		build: func(t *testing.T, run *Run) {
			mustAppend(t, run, Event{Type: EventRunFinished, Status: string(PhaseFailed)})
		},
		// Comfortably past runPhaseTailBudget in records the scan cannot decide on.
		mangle: padJournal(200, 64*1024),
	})

	if _, _, _, err := tailPhaseContext(context.Background(), filepath.Join(dir, fileEvents)); err == nil {
		t.Fatal("expected the bounded scan to exhaust its budget on this journal")
	} else if !errors.Is(err, errPhaseTailBudgetExhausted) {
		t.Fatalf("tailPhase error = %v, want budget exhaustion", err)
	}

	rd, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	phase, err := rd.PhaseBounded(context.Background())
	if err != nil {
		t.Fatalf("PhaseBounded: %v", err)
	}
	if phase != PhaseFailed {
		t.Errorf("PhaseBounded = %q, want %q — the fallback did not run", phase, PhaseFailed)
	}
}

func TestPhaseBoundedContextFallbackAgreesWithPhase(t *testing.T) {
	dir := buildRunPhaseFixture(t, runPhaseFixture{
		build: func(t *testing.T, run *Run) {
			mustAppend(t, run, Event{Type: EventRunFinished, Status: string(PhaseFailed)})
		},
		mangle: padJournal(200, 64*1024),
	})
	rd, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	phase, err := rd.PhaseBounded(context.Background())
	if err != nil {
		t.Fatalf("PhaseBounded: %v", err)
	}
	if phase != PhaseFailed {
		t.Errorf("PhaseBounded = %q, want %q", phase, PhaseFailed)
	}
}

func TestPhaseFallbackChecksCancellationBetweenReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelAfterRead{
		reader: bytes.NewReader(append(bytes.Repeat([]byte("x"), runPhaseChunkSize), '\n')),
		cancel: cancel,
	}
	_, _, err := phaseFromReaderContext(ctx, reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("phaseFromReaderContext error = %v, want context.Canceled", err)
	}
	if reader.reads != 1 {
		t.Fatalf("reader completed %d reads after cancellation, want 1", reader.reads)
	}
}

// TestPhaseBoundedReadsBoundedBytes is the reason this exists at all: the
// terminal run that dominates a long-lived instance must be decided from its
// tail, not from its whole journal. Asserted in bytes read rather than in wall
// time, because a fast machine hides an O(history) read and a slow one fails a
// latency bound that has nothing to do with this code.
func TestPhaseBoundedReadsBoundedBytes(t *testing.T) {
	dir := buildRunPhaseFixture(t, runPhaseFixture{
		build: func(*testing.T, *Run) {},
		// A journal far larger than one backward step, whose last record is the
		// run.finished every terminal run ends on — the shape that dominates a
		// long-lived instance and the one the boot scan must not read whole.
		mangle: padJournal(80, 4096, Event{Type: EventRunFinished, Status: string(PhaseCompleted)}),
	})
	eventsPath := filepath.Join(dir, fileEvents)

	info, err := os.Stat(eventsPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() < 4*runPhaseChunkSize {
		t.Fatalf("fixture journal is %d bytes, too small to show a bound", info.Size())
	}

	phase, decided, read, err := tailPhaseContext(context.Background(), eventsPath)
	if err != nil || !decided {
		t.Fatalf("tailPhase = %q, %v, %v", phase, decided, err)
	}
	if phase != PhaseCompleted {
		t.Fatalf("tailPhase = %q, want %q", phase, PhaseCompleted)
	}
	// One backward step is enough for a run whose last record is run.finished,
	// and that is the whole point.
	if read > runPhaseChunkSize {
		t.Errorf("bounded scan read %d bytes of a %d-byte journal; want at most one %d-byte step",
			read, info.Size(), runPhaseChunkSize)
	}
}

func buildRunPhaseFixture(t *testing.T, tc runPhaseFixture) string {
	t.Helper()
	run, root := newRun(t)
	tc.build(t, run)
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	dir := filepath.Join(root, testIdentity().RunID)
	if tc.mangle != nil {
		tc.mangle(t, dir)
	}
	return dir
}

func mustAppend(t *testing.T, run *Run, ev Event) {
	t.Helper()
	if err := run.Append(ev); err != nil {
		t.Fatalf("Append(%s): %v", ev.Type, err)
	}
}

// padJournal returns a mangler that appends n non-decisive records of roughly
// size bytes each, so a fixture can put real distance between the decisive
// record and the end of the journal.
//
// The records are written straight to events.jsonl rather than through Append.
// Append fsyncs and rewrites the checkpoint per event, which turns a few hundred
// filler records into ten seconds of test — and none of that machinery is under
// test here. What is under test is how the backward scan behaves over bytes on
// disk, and these are the same bytes Append would have produced.
func padJournal(n, size int, trailing ...Event) func(*testing.T, string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		filler := strings.Repeat("p", size)
		records := make([]Event, 0, n+len(trailing))
		for i := range n {
			records = append(records, Event{
				Type:   EventStageFinished,
				Target: fmt.Sprintf("pad-%d", i),
				Runner: map[string]any{"filler": filler},
			})
		}
		records = append(records, trailing...)
		var buf bytes.Buffer
		for _, ev := range records {
			ev.Schema = EventSchema
			line, err := json.Marshal(ev)
			if err != nil {
				t.Fatalf("marshal pad record: %v", err)
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		appendRawToEvents(t, dir, buf.Bytes())
	}
}

func appendRawToEvents(t *testing.T, dir string, raw []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, fileEvents), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open events log: %v", err)
	}
	if _, err := f.Write(raw); err != nil {
		t.Fatalf("write raw bytes: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close events log: %v", err)
	}
}

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
	reads  int
}

func (r *cancelAfterRead) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.reads++
	r.cancel()
	return n, err
}
