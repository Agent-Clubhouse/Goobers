package journal

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRecoverTruncationProperty exhaustively truncates a valid event log at
// every byte offset and asserts recovery is consistent: every completed
// (newline-terminated) event survives, a partial tail is discarded, and seqs
// stay contiguous. This is the deterministic core of the kill-9 property — a
// crash can only ever leave a partial final record, which is what truncation at
// an arbitrary offset simulates.
func TestRecoverTruncationProperty(t *testing.T) {
	// Build a canonical, valid log. A handful of events is enough to span
	// several record boundaries; keeping it small keeps the every-offset sweep
	// (below) fast despite the per-recovery fsync.
	src := t.TempDir()
	run, err := Create(src, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := run.Append(Event{Type: EventStageStarted, Stage: "s", Attempt: i + 1}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	_ = run.Close()

	srcDir := filepath.Join(src, testIdentity().RunID)
	full, err := os.ReadFile(filepath.Join(srcDir, fileEvents))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	wantAll, _, err := readEvents(filepath.Join(srcDir, fileEvents))
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	// Reuse one run.yaml for every truncated copy; write test fixtures with
	// os.WriteFile (no fsync) so only the code-under-test's durability writes
	// dominate the timing.
	runYAML, err := os.ReadFile(filepath.Join(srcDir, fileRunYAML))
	if err != nil {
		t.Fatalf("read run.yaml: %v", err)
	}

	base := t.TempDir()
	for tlen := 0; tlen <= len(full); tlen++ {
		dir := filepath.Join(base, strconv.Itoa(tlen))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fileRunYAML), runYAML, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fileEvents), full[:tlen], 0o644); err != nil {
			t.Fatal(err)
		}

		// Expected: every event whose terminating newline is within [:tlen].
		wantCount := bytes.Count(full[:tlen], []byte{'\n'})
		tornExpected := tlen - lastNewlineEnd(full[:tlen])

		run, report, err := Recover(dir, WithClock(fixedClock()))
		if err != nil {
			t.Fatalf("tlen=%d Recover: %v", tlen, err)
		}
		_ = run.Close()

		if report.TornBytes != tornExpected {
			t.Fatalf("tlen=%d: tornBytes=%d want %d", tlen, report.TornBytes, tornExpected)
		}
		if report.Repaired != (tornExpected > 0) {
			t.Fatalf("tlen=%d: repaired=%v want %v", tlen, report.Repaired, tornExpected > 0)
		}

		rd, _ := OpenRead(dir)
		got, err := rd.Events()
		if err != nil {
			t.Fatalf("tlen=%d Events after recover: %v", tlen, err)
		}
		// No completed event is ever lost.
		if len(got) < wantCount {
			t.Fatalf("tlen=%d: lost completed events: got %d want >= %d", tlen, len(got), wantCount)
		}
		// The completed prefix matches the original exactly.
		for i := 0; i < wantCount; i++ {
			if got[i].Seq != wantAll[i].Seq || got[i].Type != wantAll[i].Type {
				t.Fatalf("tlen=%d event %d drifted: got {seq %d %s} want {seq %d %s}",
					tlen, i, got[i].Seq, got[i].Type, wantAll[i].Seq, wantAll[i].Type)
			}
		}
		// If repaired, exactly one corrective event was appended after the prefix.
		if tornExpected > 0 {
			if len(got) != wantCount+1 || got[wantCount].Type != EventRepaired {
				t.Fatalf("tlen=%d: expected a trailing repaired event, got %d events", tlen, len(got))
			}
		}
		// Seqs are contiguous from 1.
		for i, ev := range got {
			if ev.Seq != uint64(i+1) {
				t.Fatalf("tlen=%d: non-contiguous seq at %d: %d", tlen, i, ev.Seq)
			}
		}
		// Recovery is idempotent: a second Recover finds nothing torn. Sampled
		// (fsync-heavy) rather than run at every offset.
		if tlen%7 == 0 {
			run2, report2, err := Recover(dir, WithClock(fixedClock()))
			if err != nil {
				t.Fatalf("tlen=%d second Recover: %v", tlen, err)
			}
			_ = run2.Close()
			if report2.TornBytes != 0 || report2.Repaired {
				t.Fatalf("tlen=%d: second recover not clean: %+v", tlen, report2)
			}
		}
	}
}

// lastNewlineEnd returns the offset just past the last '\n' in b (0 if none).
func lastNewlineEnd(b []byte) int {
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return i + 1
	}
	return 0
}

const (
	crashChildEnv = "GO_JOURNAL_CRASH_CHILD"
	crashRootEnv  = "GO_JOURNAL_CRASH_ROOT"
)

// TestKill9MidAppendRecovers spawns a real child process that appends events in
// a tight loop, SIGKILLs it mid-stream (os.Process.Kill == kill -9), and asserts
// the journal recovers to a consistent state with no lost completed events. This
// is the literal acceptance criterion; the exhaustive truncation test above is
// its deterministic complement.
func TestKill9MidAppendRecovers(t *testing.T) {
	if os.Getenv(crashChildEnv) != "" {
		runCrashChild() // never returns
		return
	}
	if testing.Short() {
		t.Skip("skipping subprocess kill-9 test in -short mode")
	}

	recovered := 0
	for iter := 0; iter < 6; iter++ {
		root := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=TestKill9MidAppendRecovers")
		cmd.Env = append(os.Environ(), crashChildEnv+"=1", crashRootEnv+"="+root)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
		_ = cmd.Process.Kill() // SIGKILL
		_ = cmd.Wait()

		dir := filepath.Join(root, testIdentity().RunID)
		if _, err := os.Stat(filepath.Join(dir, fileEvents)); err != nil {
			continue // child killed before it created the log; retry
		}
		run, report, err := Recover(dir)
		if err != nil {
			t.Fatalf("iter %d Recover: %v", iter, err)
		}
		_ = run.Close()

		rd, _ := OpenRead(dir)
		got, err := rd.Events()
		if err != nil {
			t.Fatalf("iter %d Events after kill-9: %v", iter, err)
		}
		if len(got) == 0 {
			continue
		}
		recovered++
		// Consistent state: contiguous seqs, run.started first, no torn tail left.
		if got[0].Type != EventRunStarted {
			t.Fatalf("iter %d: first event is %s, not run.started", iter, got[0].Type)
		}
		for i, ev := range got {
			if ev.Seq != uint64(i+1) {
				t.Fatalf("iter %d: non-contiguous seq at %d: %d (torn=%d)", iter, i, ev.Seq, report.TornBytes)
			}
		}
		// Second recovery is clean — the torn tail was fully repaired.
		run2, report2, err := Recover(dir)
		if err != nil {
			t.Fatalf("iter %d second Recover: %v", iter, err)
		}
		_ = run2.Close()
		if report2.TornBytes != 0 {
			t.Fatalf("iter %d: torn tail survived first recovery: %+v", iter, report2)
		}
	}
	if recovered == 0 {
		t.Skip("child never durably wrote before kill; no recovery exercised")
	}
}

// runCrashChild is the subprocess body: create a run and append forever until
// SIGKILLed. Padding makes each append span enough bytes to be interruptible
// mid-write.
func runCrashChild() {
	root := os.Getenv(crashRootEnv)
	run, err := Create(root, testIdentity(), nil)
	if err != nil {
		os.Exit(2)
	}
	pad := strings.Repeat("x", 512)
	for i := 0; ; i++ {
		if err := run.Append(Event{
			Type:    EventStageStarted,
			Stage:   "loop",
			Attempt: i + 1,
			Runner:  map[string]any{"pad": pad},
		}); err != nil {
			os.Exit(3)
		}
	}
}
