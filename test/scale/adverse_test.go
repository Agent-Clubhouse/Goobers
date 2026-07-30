package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Adverse-state fixtures.
//
// §16.4 asks the harness to cover "slow disk, low-core hardware, disk-full,
// read-only volume, corrupt store, daemon<->standalone transitions", and §14.8
// makes crash/restart/adverse-state safety a property with a test rather than an
// aspiration.
//
// These matter because every one of them is a state the *portal* has to have an
// answer for. Design §7.2 allows exactly three response states — current, stale
// by a stated amount, unavailable with a reason — and "a fourth indefinite one"
// is the failure the whole read contract exists to prevent. An adverse state
// that produces a hang, a silent empty list, or an unexplained 500 is that
// fourth state, and there is no way to know which without inducing them.
//
// Each fixture below returns a cleanup, so a test can assert behavior *in* the
// adverse state and then confirm recovery once it clears — recovery being the
// half that a one-way fault injection cannot check.

// AdverseState names an induced fault.
type AdverseState string

const (
	// AdverseReadOnlyVolume models the instance directory losing write
	// permission — a full disk quota, a read-only remount, or the deliberately
	// read-only volume §11.2 says standalone must serve degraded rather than
	// silently scanning.
	AdverseReadOnlyVolume AdverseState = "read-only-volume"
	// AdverseCorruptStore models telemetry.db being truncated or garbage. The
	// read model is derived and rebuildable, so the required behavior is a
	// stated failure and a rebuild, never a wrong answer.
	AdverseCorruptStore AdverseState = "corrupt-store"
	// AdverseCorruptJournalTail models a torn final record — the crash shape the
	// journal's recovery path exists for, and the one #116's NUL cascade turns
	// into a sequence-allocation hazard (§14.11).
	AdverseCorruptJournalTail AdverseState = "corrupt-journal-tail"
	// AdverseMissingJournal models a run directory that is indexed but whose
	// journal has been removed — an operator `rm`, a failed restore, or an
	// unlink whose removal intent was lost. §6.3 requires repair to reconcile
	// this direction too, and §11.4 calls a projected row outliving its journal
	// "impossible", which is only true once that reconciliation exists.
	AdverseMissingJournal AdverseState = "missing-journal"
)

// AdverseFixture is an induced fault plus the means to clear it.
type AdverseFixture struct {
	State   AdverseState
	Cleanup func()
}

// makeReadOnlyVolume removes write permission from the instance's run roots.
//
// Skipped when running as root, which can write to a mode-0555 directory
// regardless — a fixture that silently does nothing is worse than one that
// declines, because the test would pass while asserting against a state it never
// entered.
func makeReadOnlyVolume(root string) (AdverseFixture, error) {
	if os.Geteuid() == 0 {
		return AdverseFixture{}, fmt.Errorf("read-only fixture is meaningless as root: mode bits do not restrict uid 0")
	}
	if runtime.GOOS == "windows" {
		return AdverseFixture{}, fmt.Errorf("read-only fixture uses POSIX mode bits; not applicable on windows")
	}
	var restored []string
	restore := func() {
		for _, dir := range restored {
			_ = os.Chmod(dir, 0o755)
		}
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree is itself part of the fault being induced
		}
		if chmodErr := os.Chmod(path, 0o555); chmodErr != nil {
			return chmodErr
		}
		restored = append(restored, path)
		return nil
	})
	if err != nil {
		restore()
		return AdverseFixture{}, fmt.Errorf("make read-only: %w", err)
	}
	return AdverseFixture{State: AdverseReadOnlyVolume, Cleanup: restore}, nil
}

// corruptStore overwrites telemetry.db with bytes that are not a SQLite file,
// preserving the original so the fixture can be cleared.
//
// Truncation is deliberately not used: a zero-length file is a *valid* empty
// database to SQLite, so it induces "no rows" rather than "corrupt", and those
// have to be distinguishable — one is a legitimate empty instance and the other
// is data loss the portal must report rather than render.
func corruptStore(dbPath string) (AdverseFixture, error) {
	original, err := os.ReadFile(dbPath)
	if err != nil {
		return AdverseFixture{}, fmt.Errorf("read store before corrupting: %w", err)
	}
	garbage := make([]byte, len(original))
	for i := range garbage {
		garbage[i] = byte(i % 251)
	}
	// A valid SQLite file starts with "SQLite format 3\x00"; make sure this does
	// not, so the failure is recognised at open rather than at first query.
	copy(garbage, []byte("NOT-A-SQLITE-FILE"))
	if err := os.WriteFile(dbPath, garbage, 0o644); err != nil {
		return AdverseFixture{}, fmt.Errorf("corrupt store: %w", err)
	}
	return AdverseFixture{
		State:   AdverseCorruptStore,
		Cleanup: func() { _ = os.WriteFile(dbPath, original, 0o644) },
	}, nil
}

// corruptJournalTail appends a torn partial record plus NUL zero-fill to a run's
// event log, reproducing the crash shape the recovery path exists for.
//
// The NUL fill is the important half. readEventRecords strips leading NUL crash
// fill and skips a line that collapses to empty, so the #116 cascade can leave a
// newline-terminated fill-only tail *after* the last valid event — which is
// exactly why §17 Wave 1.1 must scan back to a line that parses with a non-zero
// Seq rather than to the last newline. A fixture with only a truncated JSON
// object would not exercise that.
func corruptJournalTail(runDir string) (AdverseFixture, error) {
	path := filepath.Join(runDir, "events.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		return AdverseFixture{}, fmt.Errorf("read journal before corrupting: %w", err)
	}
	torn := append([]byte(nil), original...)
	// A newline-terminated line of pure NUL fill, then a partial record with no
	// trailing newline.
	torn = append(torn, make([]byte, 512)...)
	torn = append(torn, '\n')
	torn = append(torn, []byte(`{"schema":"goobers.dev/journal/v1","seq":`)...)
	if err := os.WriteFile(path, torn, 0o644); err != nil {
		return AdverseFixture{}, fmt.Errorf("corrupt journal tail: %w", err)
	}
	return AdverseFixture{
		State:   AdverseCorruptJournalTail,
		Cleanup: func() { _ = os.WriteFile(path, original, 0o644) },
	}, nil
}

// removeJournal deletes a run's event log while leaving its run.yaml, so the run
// is still discoverable and indexable but no longer readable.
func removeJournal(runDir string) (AdverseFixture, error) {
	path := filepath.Join(runDir, "events.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		return AdverseFixture{}, fmt.Errorf("read journal before removing: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return AdverseFixture{}, fmt.Errorf("remove journal: %w", err)
	}
	return AdverseFixture{
		State:   AdverseMissingJournal,
		Cleanup: func() { _ = os.WriteFile(path, original, 0o644) },
	}, nil
}
