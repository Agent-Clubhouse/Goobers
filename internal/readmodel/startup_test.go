package readmodel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// TestEnsureReadyRestartOpensOnlyStaleJournals pins #6895: a restart must open
// journals in proportion to the rows that need it, not to the instance's
// history. One unrecoverable stale row used to force a whole-journal rebuild
// on every restart.
func TestEnsureReadyRestartOpensOnlyStaleJournals(t *testing.T) {
	ctx := context.Background()
	roots := []string{filepath.Join(t.TempDir(), "alpha"), filepath.Join(t.TempDir(), "beta")}
	const runsPerRoot = 60
	for i := 0; i < runsPerRoot; i++ {
		for r, root := range roots {
			writeRunJournal(t, root, fmt.Sprintf("run-%d-%03d", r, i), false)
		}
	}
	path := filepath.Join(t.TempDir(), FileName)

	store, opened, lines := openCountingStore(t, path)
	result := ensureReady(t, store, roots, lines)
	if !result.FullBuild || *opened != 2*runsPerRoot || result.Projected != 2*runsPerRoot {
		t.Fatalf("first build: result=%+v opened=%d, want a full build of %d runs", result, *opened, 2*runsPerRoot)
	}
	if want := fmt.Sprintf("read model build: scanned %d/%d run directories", 2*runsPerRoot, 2*runsPerRoot); !slices.Contains(*lines, want) {
		t.Fatalf("build progress = %q, want %q", *lines, want)
	}
	closeStore(t, store)

	// Nothing changed: a restart opens no journals and says nothing.
	store, opened, lines = openCountingStore(t, path)
	if result := ensureReady(t, store, roots, lines); result.FullBuild || *opened != 0 || len(*lines) != 0 {
		t.Fatalf("clean restart: result=%+v opened=%d lines=%q, want no work", result, *opened, *lines)
	}

	// Three rows predate the projection rules (one with a derived phase only
	// its journal can correct) and a fourth has lost its journal.
	stale := []string{"run-0-007", "run-1-031", "run-1-059", "run-0-042"}
	for _, runID := range stale {
		if _, err := store.writer.ExecContext(ctx,
			`UPDATE run SET projection_version = ? WHERE run_id = ?`, currentProjectionVersion-1, runID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.writer.ExecContext(ctx,
		`UPDATE run SET phase = ?, terminal = 0 WHERE run_id = ?`, string(journal.PhaseRunning), stale[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(roots[0], stale[3])); err != nil {
		t.Fatal(err)
	}
	closeStore(t, store)

	store, opened, lines = openCountingStore(t, path)
	readyDuringPass := false
	store.projectDirObserver = func(string) {
		*opened++
		if state, err := store.State(ctx); err != nil || state.Ready {
			readyDuringPass = true
		}
	}
	result = ensureReady(t, store, roots, lines)
	if result.FullBuild || *opened != 3 || result.Considered != 4 || result.Projected != 3 || result.Unresolved != 1 {
		t.Fatalf("restart: result=%+v opened=%d, want 3 of 4 stale rows re-projected and no full scan", result, *opened)
	}
	if readyDuringPass {
		t.Fatal("store reported ready while mixing old- and new-rule rows")
	}
	if state, err := store.State(ctx); err != nil || !state.Ready {
		t.Fatalf("state after re-projection = %+v err=%v, want ready", state, err)
	}
	if !slices.Contains(*lines, "read model re-projection: 4/4 runs") {
		t.Fatalf("re-projection progress = %q, want a 4/4 line", *lines)
	}
	if run, ok, err := store.GetRun(ctx, stale[0]); err != nil || !ok || run.Phase != journal.PhaseCompleted {
		t.Fatalf("stale row = %+v found=%v err=%v, want the journal's completed phase", run, ok, err)
	}
	var version int
	if err := store.writer.QueryRowContext(ctx,
		`SELECT projection_version FROM run WHERE run_id = ?`, stale[1]).Scan(&version); err != nil || version != currentProjectionVersion {
		t.Fatalf("re-projected version = %d err=%v, want %d", version, err, currentProjectionVersion)
	}
	closeStore(t, store)

	// The row without a journal stays stale rather than being marked current,
	// and costs the next restart no journal opens.
	store, opened, _ = openCountingStore(t, path)
	if result := ensureReady(t, store, roots, nil); result.FullBuild || *opened != 0 || result.Unresolved != 1 {
		t.Fatalf("second restart: result=%+v opened=%d, want only the unresolved row considered", result, *opened)
	}
}

// TestEnsureReadyUnreadyStoreStillBuildsEveryJournal keeps the whole-journal
// build for the stores that need it: an interrupted build or a migration that
// withdrew readiness to replay every run.
func TestEnsureReadyUnreadyStoreStillBuildsEveryJournal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeRunJournal(t, root, fmt.Sprintf("run-%d", i), false)
	}
	path := filepath.Join(t.TempDir(), FileName)
	store, _, _ := openCountingStore(t, path)
	ensureReady(t, store, []string{root}, nil)
	if _, err := store.writer.ExecContext(ctx, `UPDATE projection_state SET ready = 0 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	closeStore(t, store)

	store, opened, _ := openCountingStore(t, path)
	if result := ensureReady(t, store, []string{root}, nil); !result.FullBuild || *opened != 5 {
		t.Fatalf("unready restart: result=%+v opened=%d, want a full build of 5 runs", result, *opened)
	}
	if state, err := store.State(ctx); err != nil || !state.Ready {
		t.Fatalf("state after build = %+v err=%v, want ready", state, err)
	}
}

// TestEnsureReadyInterruptedReprojectionLeavesStoreUnready proves a pass cut
// short cannot leave old-rule rows behind a ready flag.
func TestEnsureReadyInterruptedReprojectionLeavesStoreUnready(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		writeRunJournal(t, root, fmt.Sprintf("run-%d", i), false)
	}
	path := filepath.Join(t.TempDir(), FileName)
	store, _, _ := openCountingStore(t, path)
	ensureReady(t, store, []string{root}, nil)
	if _, err := store.writer.ExecContext(context.Background(),
		`UPDATE run SET projection_version = ?`, currentProjectionVersion-1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.projectDirObserver = func(string) { cancel() }
	state, err := store.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureReady(ctx, state, []string{root}, nil); err == nil {
		t.Fatal("interrupted re-projection reported success")
	}
	if state, err := store.State(context.Background()); err != nil || state.Ready {
		t.Fatalf("state after interrupted pass = %+v err=%v, want unready", state, err)
	}
}

func openCountingStore(t *testing.T, path string) (*Store, *int, *[]string) {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	opened := new(int)
	store.projectDirObserver = func(string) { *opened++ }
	return store, opened, new([]string)
}

func ensureReady(t *testing.T, store *Store, roots []string, lines *[]string) ReadyResult {
	t.Helper()
	ctx := context.Background()
	state, err := store.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var report func(string)
	if lines != nil {
		report = func(line string) { *lines = append(*lines, line) }
	}
	result, err := store.EnsureReady(ctx, state, roots, report)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func closeStore(t *testing.T, store *Store) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
