package journal

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// constClock is a race-free fixed clock for concurrency tests — unlike
// fixedClock() it holds no mutable counter, so many goroutines may read it at
// once without a data race in the test harness itself.
func constClock() func() time.Time {
	t := time.Date(2026, 7, 13, 5, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// TestNilScrubberFailsClosed is the #118 negative control for the WithScrubber(nil)
// fail-open: a nil scrubber must fall back to real redaction (the pattern net),
// never nopScrubber. A pattern-detectable token fed through a run created with
// WithScrubber(nil) must not land at rest.
func TestNilScrubberFailsClosed(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithScrubber(nil), WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789AB" // pattern-net detectable
	if err := run.Append(Event{
		Type:  EventError,
		Error: &ErrorDetail{Code: "leak", Message: "token " + token},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_ = run.Close()

	dir := filepath.Join(root, testIdentity().RunID)
	if hits := filesContaining(t, dir, []byte(token)); len(hits) > 0 {
		t.Fatalf("WithScrubber(nil) failed open — token left at rest in: %v", hits)
	}
}

// TestConcurrentCreateSameRunRejectsAllButOne asserts the concurrency property
// the atomic-os.Mkdir fix guarantees: N goroutines racing to create the same run
// id yield exactly one winner, the rest a clean "already exists" error — never
// two writers interleaving on one journal. It runs under -race for data-race
// safety and guards against a regression that reintroduces a non-atomic create.
// (It is not a reproduced-race control: the pre-fix Stat→MkdirAll window is a
// couple of instructions wide and does not reproduce from Go-level concurrency
// even at N in the hundreds — the fix is correct by construction.)
func TestConcurrentCreateSameRunRejectsAllButOne(t *testing.T) {
	root := t.TempDir()
	const n = 24
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			run, err := Create(root, testIdentity(), nil, WithClock(constClock()))
			if err == nil {
				atomic.AddInt64(&wins, 1)
				_ = run.Close()
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("expected exactly 1 Create to win the race, got %d", wins)
	}
}

// TestRegistryScrubberConcurrentRegisterAndScrub exercises the "safe for
// concurrent use" claim on RegistryScrubber: registrations and scrubs run
// concurrently. Run under -race, it certifies the RWMutex; the final assertion
// confirms a registered secret is still redacted afterward.
func TestRegistryScrubberConcurrentRegisterAndScrub(t *testing.T) {
	reg := NewRegistryScrubber()
	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(k int) {
			defer wg.Done()
			reg.Register([]byte(fmt.Sprintf("registered-secret-%06d", k)))
		}(i)
		go func(k int) {
			defer wg.Done()
			_ = reg.Scrub([]byte(fmt.Sprintf("text registered-secret-%06d more", k)))
		}(i)
	}
	wg.Wait()

	out := reg.Scrub([]byte("value registered-secret-000001 end"))
	if string(out) == "value registered-secret-000001 end" {
		t.Fatalf("a registered secret was not redacted after concurrent use: %q", out)
	}
}

// TestRunConcurrentAppendAndRecordArtifact exercises the "safe for concurrent
// use" claim on Run: concurrent Append and RecordArtifact calls. Under -race it
// certifies the run mutex; the assertions confirm every event landed with a
// unique, contiguous seq (no lost or duplicated writes).
func TestRunConcurrentAppendAndRecordArtifact(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(constClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			if k%2 == 0 {
				if e := run.Append(Event{Type: EventStageStarted, Stage: "s", Attempt: k}); e != nil {
					errs <- e
				}
			} else {
				if _, e := run.RecordArtifact(fmt.Sprintf("a-%d", k), []byte(fmt.Sprintf("data-%d", k))); e != nil {
					errs <- e
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent op failed: %v", e)
	}
	_ = run.Close()

	rd, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// run.started + n concurrent events, all seqs unique and contiguous from 1.
	if len(events) != n+1 {
		t.Fatalf("expected %d events, got %d", n+1, len(events))
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("non-contiguous/duplicated seq at index %d: %d", i, ev.Seq)
		}
	}
}
