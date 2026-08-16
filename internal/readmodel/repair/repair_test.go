package repair

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
)

func openStore(t *testing.T) *readmodel.Store {
	t.Helper()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// writeRun creates a minimal recorded run directory under root.
//
// The run's started_at comes from the journal itself (wall clock at creation),
// which is why the floor tests place the FLOOR relative to now rather than
// backdating the run: the journal writer has no clock seam, and reaching into
// one would test the fixture rather than the sweep.
func writeRun(t *testing.T, root, runID string) string {
	t.Helper()
	writer, err := journal.Create(root, journal.RunIdentity{
		RunID: runID, Gaggle: "alpha", Workflow: "wf", WorkflowVersion: 1,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create journal for %s: %v", runID, err)
	}
	if err := writer.Append(journal.Event{
		Type: journal.EventStageStarted, Stage: "build", Attempt: 1,
	}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := writer.Append(journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatalf("append run.finished: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	return filepath.Join(root, runID)
}

// TestSweepDiscoversAnUnprojectedRun is the forward direction.
func TestSweepDiscoversAnUnprojectedRun(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	root := t.TempDir()
	writeRun(t, root, fmt.Sprintf("%032x", 1))

	sweeper := New(store, store, nil, Options{RunsDirs: []string{root}, BatchSize: 10})
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}
	if _, ok, _ := store.GetRun(ctx, fmt.Sprintf("%032x", 1)); !ok {
		t.Error("the sweep did not project a run that exists on disk")
	}
}

// TestSweepNeverCreatesALockFile is #1924's headline acceptance criterion.
//
// The previous reconcile ran on the HTTP list path and reached IngestRun ->
// WithPruneProtection -> acquireJournalLock, which is why all 40,665 run
// directories on the live instance contain a .lock file — including the 10,906
// with no run.yaml that can never be ingested. Every one was created by a read.
func TestSweepNeverCreatesALockFile(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeRun(t, root, fmt.Sprintf("%032x", i))
	}
	// An unpublished directory too — historically the worst case, since it could
	// never be ingested yet still got locked.
	if err := os.MkdirAll(filepath.Join(root, "unpublished-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	before := countLocks(t, root)
	sweeper := New(store, store, nil, Options{RunsDirs: []string{root}, BatchSize: 50})
	for i := 0; i < 3; i++ {
		if err := sweeper.Step(ctx); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if after := countLocks(t, root); after != before {
		t.Errorf("the sweep created %d lock files (before %d, after %d); repair must never "+
			"take a journal lock", after-before, before, after)
	}
}

func countLocks(t *testing.T, root string) int {
	t.Helper()
	var n int
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() && filepath.Base(path) == ".lock" {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSweepRemovesAProjectedRunWhoseJournalIsGone is the reverse direction, and
// the fix for #1943.
//
// A run whose journal is removed is currently reclassified as running and stays
// that way forever, because nothing looks for rows whose source has vanished.
// Without this direction, "a projected row cannot outlive its journal" is a hope
// rather than a property.
func TestSweepRemovesAProjectedRunWhoseJournalIsGone(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	root := t.TempDir()

	runID := fmt.Sprintf("%032x", 7)
	startedAt := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := store.UpsertRun(ctx, readmodel.Projection{Run: readmodel.RunRow{
		RunID: runID, Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseRunning, Terminal: false,
		StartedAt: startedAt, LastSeq: 3,
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Its journal never existed on disk — the operator-rm case.
	sweeper := New(store, store, nil, Options{
		RunsDirs: []string{root}, BatchSize: 10,
		Now: func() time.Time { return startedAt.Add(time.Hour) },
	})
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}

	if _, ok, _ := store.GetRun(ctx, runID); ok {
		t.Error("a projected run with no journal on disk survived the sweep; #1943's " +
			"phantom running run stays visible forever")
	}
	changes, err := store.Changes(ctx, 0, 10)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	var removed bool
	for _, change := range changes {
		if change.Kind == readmodel.ChangeRunRemoved && change.RunID == runID {
			removed = true
		}
	}
	if !removed {
		t.Error("no run.removed change was published; a connected client would keep showing " +
			"the run with no way to learn it is gone")
	}
}

// TestSweepSkipsAndTombstonesBelowTheFloor pins the livelock prevention.
//
// Without a floor, repair reprojects an aged-out run, retention deletes it, and
// the next cycle repeats — consuming the whole budget and flooding the change
// feed.
func TestSweepSkipsAndTombstonesBelowTheFloor(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	root := t.TempDir()

	// The floor is placed AHEAD of the run rather than the run behind the floor:
	// the journal writer stamps started_at from the wall clock with no seam, and
	// reaching into it would test the fixture instead of the sweep.
	runID := fmt.Sprintf("%032x", 11)
	writeRun(t, root, runID)
	floor := time.Now().UTC().Add(time.Hour)
	if err := store.SetProjectionFloor(ctx, floor); err != nil {
		t.Fatalf("set floor: %v", err)
	}

	sweeper := New(store, store, nil, Options{RunsDirs: []string{root}, BatchSize: 10})
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}
	if _, ok, _ := store.GetRun(ctx, runID); ok {
		t.Error("a run below the projection floor was re-admitted; retention will delete it " +
			"and the next cycle will re-admit it again")
	}
	if tombstoned, _ := store.Tombstoned(ctx, runID); !tombstoned {
		t.Error("the aged-out run was not tombstoned, so 'missing' and 'deliberately gone' " +
			"remain the same observation")
	}

	// A second pass must not re-examine it — that is what the tombstone is for.
	statsBefore := sweeper.Stats().Projected
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if sweeper.Stats().Projected != statsBefore {
		t.Error("the tombstoned run was projected on a later pass")
	}
}

// TestResumeOverridesTheFloor pins the one case that re-admits a pre-floor run.
//
// runner.ResumeFromTerminal durably reopens an escalated or failed run whose
// journal may predate the window. An intake marker is authority to re-admit:
// refusing would make a human action invisible in the portal that prompted it.
func TestResumeOverridesTheFloor(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	root := t.TempDir()

	runID := fmt.Sprintf("%032x", 13)
	writeRun(t, root, runID)
	floor := time.Now().UTC().Add(time.Hour)
	if err := store.SetProjectionFloor(ctx, floor); err != nil {
		t.Fatalf("set floor: %v", err)
	}

	sweeper := New(store, store, markerFor(runID), Options{RunsDirs: []string{root}, BatchSize: 10})
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}
	if _, ok, _ := store.GetRun(ctx, runID); !ok {
		t.Error("a resumed pre-floor run was not re-admitted; the human action that resumed " +
			"it is invisible in the portal that prompted it")
	}
}

// TestUnpublishedIsRememberedByMtimeAndForgottenOnPromotion pins the memo and
// its self-invalidation.
//
// 10,906 of 40,665 directories on the live instance have no run.yaml and can
// never be ingested. Remembering them makes each cost one stat per cycle. The
// mtime key is what stops that being permanent: writing run.yaml bumps the
// directory mtime, so a promoted run no longer matches its memo.
func TestUnpublishedIsRememberedByMtimeAndForgottenOnPromotion(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	root := t.TempDir()

	runID := fmt.Sprintf("%032x", 17)
	dir := filepath.Join(root, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	sweeper := New(store, store, nil, Options{RunsDirs: []string{root}, BatchSize: 10})
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}
	if sweeper.Stats().SkippedUnpub == 0 {
		t.Fatal("an unpublished directory was not recorded as such")
	}
	remembered, err := store.IsUnpublished(ctx, runID, dirMtime(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if !remembered {
		t.Fatal("the unpublished memo was not written; the 27% would cost an open every cycle")
	}

	// Promote it: writing run.yaml is exactly what a publish does, and it bumps
	// the directory's mtime. Written directly rather than via journal.Create,
	// which refuses a directory that already exists — the promotion case is
	// precisely "this directory was here before it was a run".
	promoteDirectory(t, root, runID)

	if remembered, err := store.IsUnpublished(ctx, runID, dirMtime(t, dir)); err != nil {
		t.Fatal(err)
	} else if remembered {
		t.Error("the memo still matches after the directory's mtime changed; a promoted run " +
			"would never be re-examined")
	}

	if err := New(store, store, nil, Options{RunsDirs: []string{root}, BatchSize: 10}).Step(ctx); err != nil {
		t.Fatalf("step after promotion: %v", err)
	}
	if _, ok, _ := store.GetRun(ctx, runID); !ok {
		t.Error("a directory promoted from unpublished was never projected")
	}
}

// promoteDirectory turns an existing plain directory into a recorded run by
// writing the journal files a publish would.
func promoteDirectory(t *testing.T, root, runID string) {
	t.Helper()
	staging := t.TempDir()
	writeRun(t, staging, runID)

	target := filepath.Join(root, runID)
	entries, err := os.ReadDir(filepath.Join(staging, runID))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(staging, runID, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, entry.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !journal.Recorded(target) {
		t.Fatal("promotion did not produce a recorded run")
	}
}

func dirMtime(t *testing.T, dir string) time.Time {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

// TestSweepCostPerStepIsBoundedByBatchSize is the rate bound.
//
// §6.3's requirement is that repair cost is constant per unit time while cycle
// time scales with history. The per-step bound is what delivers that: ten times
// the corpus must not make one step ten times more expensive.
func TestSweepCostPerStepIsBoundedByBatchSize(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for i := 0; i < 300; i++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("%032x", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	store := openStore(t)
	sweeper := New(store, store, nil, Options{RunsDirs: []string{root}, BatchSize: 25})
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := sweeper.Stats().EntriesExamined; got != 25 {
		t.Errorf("one step examined %d entries against a batch size of 25; the walk is not "+
			"rate-bounded, so a 40,665-directory instance pays for all of it every pass", got)
	}
}

func TestReverseSweepSharesBudgetAndResumesAfterLastProbe(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	root := t.TempDir()
	startedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	runIDs := make([]string, 8)
	for i := range runIDs {
		runIDs[i] = fmt.Sprintf("%032x", i)
		if err := os.Mkdir(filepath.Join(root, runIDs[i]), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertRun(ctx, readmodel.Projection{Run: readmodel.RunRow{
			RunID: runIDs[i], Gaggle: "alpha", Workflow: "wf",
			Phase: journal.PhaseRunning, StartedAt: startedAt,
		}}); err != nil {
			t.Fatalf("seed %s: %v", runIDs[i], err)
		}
	}

	sweeper := New(store, store, nil, Options{
		RunsDirs:  []string{root},
		BatchSize: 4,
		Now:       func() time.Time { return startedAt.Add(time.Hour) },
	})
	statCalls := make(map[string]int)
	sweeper.stat = func(path string) (os.FileInfo, error) {
		statCalls[path]++
		return os.Stat(path)
	}

	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("first step: %v", err)
	}
	if got := sweeper.Stats().EntriesExamined; got != 4 {
		t.Fatalf("first step examined %d entries, want the shared batch budget of 4", got)
	}
	oldestCalls := []int{
		statCalls[filepath.Join(root, runIDs[0])],
		statCalls[filepath.Join(root, runIDs[1])],
	}

	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("second step: %v", err)
	}
	if got := sweeper.Stats().EntriesExamined; got != 8 {
		t.Fatalf("two steps examined %d entries, want 8", got)
	}
	for i, runID := range runIDs[:2] {
		if got := statCalls[filepath.Join(root, runID)]; got != oldestCalls[i] {
			t.Errorf("second step re-probed oldest run %s: stat calls = %d, want %d",
				runID, got, oldestCalls[i])
		}
	}

	for step := 3; step <= 5; step++ {
		before := sweeper.Stats().EntriesExamined
		if err := sweeper.Step(ctx); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		if examined := sweeper.Stats().EntriesExamined - before; examined > 4 {
			t.Fatalf("step %d examined %d entries, batch budget is 4", step, examined)
		}
	}
	beforeRestart := statCalls[filepath.Join(root, runIDs[0])]
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("cycle restart step: %v", err)
	}
	if got := statCalls[filepath.Join(root, runIDs[0])]; got <= beforeRestart {
		t.Error("reverse scan did not restart from the oldest row after completing its cycle")
	}
}

func TestOneEntryBatchMakesProgressInBothDirectionsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	root := t.TempDir()
	startedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	runID := fmt.Sprintf("%032x", 1)
	if err := os.Mkdir(filepath.Join(root, runID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRun(ctx, readmodel.Projection{Run: readmodel.RunRow{
		RunID: runID, Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseRunning, StartedAt: startedAt,
	}}); err != nil {
		t.Fatal(err)
	}

	options := Options{
		RunsDirs:  []string{root},
		BatchSize: 1,
		Now:       func() time.Time { return startedAt.Add(time.Hour) },
	}
	reverse := New(store, store, nil, options)
	if err := reverse.Step(ctx); err != nil {
		t.Fatalf("reverse step: %v", err)
	}
	cursor, err := store.SweepCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ReverseAfterRunID != runID {
		t.Fatalf("reverse cursor = %q, want %q", cursor.ReverseAfterRunID, runID)
	}
	if !cursor.ForwardNext {
		t.Fatal("one-entry scheduler did not persist the forward turn")
	}

	forward := New(store, store, nil, options)
	if err := forward.Step(ctx); err != nil {
		t.Fatalf("forward step after restart: %v", err)
	}
	cursor, err = store.SweepCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.EntriesThisCycle != 1 {
		t.Fatalf("forward entries after two one-entry steps = %d, want 1", cursor.EntriesThisCycle)
	}
	if cursor.ForwardNext {
		t.Fatal("one-entry scheduler did not persist the next reverse turn")
	}
}

// TestSweepCursorResumesAcrossRestart pins that the position is durable.
//
// A fixed budget only produces a complete cycle if the walk resumes. A cursor
// held in memory would restart from the beginning on every daemon restart, and
// on a frequently-restarted instance the tail of the corpus would never be
// swept at all.
func TestSweepCursorResumesAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "read.db")
	root := t.TempDir()
	for i := 0; i < 60; i++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("%032x", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	first, err := readmodel.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sweeper := New(first, first, nil, Options{RunsDirs: []string{root}, BatchSize: 20})
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}
	cursor, err := first.SweepCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.AfterName == "" {
		t.Fatal("the cursor did not advance")
	}
	_ = first.Close()

	second, err := readmodel.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	resumed, err := second.SweepCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.AfterName != cursor.AfterName {
		t.Errorf("after restart the cursor is at %q, want %q; the walk restarts from the "+
			"beginning and the tail of the corpus is never reached",
			resumed.AfterName, cursor.AfterName)
	}
}

// markerFor is a Watermarks stub reporting one run as having intake.
type markerFor string

func (m markerFor) Get(_ context.Context, runID string) (intake.Marker, bool, error) {
	if runID != string(m) {
		return intake.Marker{}, false, nil
	}
	return intake.Marker{RunID: runID, SourceSeq: 1}, true, nil
}
