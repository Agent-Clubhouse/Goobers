//go:build integration

package main

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
)

// Model the exact crash window after durable archival and source deletion but
// before its receipt. Startup must finish this even when no archive is missing.
func interruptForkSourceRelease(t *testing.T, r parentArchiveRestorer, run *journal.Run, reader *journal.Reader) {
	t.Helper()
	pending, err := runner.PendingParentForks(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.recoverForkPlans(t.Context(), run, reader, pending); err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	at, terminal, err := recoveryCaptureWindow(t.Context(), reader, id.StartedAt)
	if err != nil || !terminal {
		t.Fatal("terminal capture boundary", err)
	}
	candidates, err := runner.ParentRetirementCandidates(reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		rec, err := runner.OwnedBranchRecorder(run, candidate.Branch)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.captureRetirement(t.Context(), reader, rec, candidate, at); err != nil {
			t.Fatal(err)
		}
	}
	retired, err := runner.RetiredParentForks(reader)
	if err != nil || len(retired) != 1 || retired[0].ReleaseRecorded {
		t.Fatal("unexpected source release before crash", retired, err)
	}
	if err := r.releaseForkSource(t.Context(), reader, retired[0]); err != nil {
		t.Fatal(err)
	}
}

func verifyChangedForkSourceRef(t *testing.T, r parentArchiveRestorer, run *journal.Run, reader *journal.Reader, archive runner.ParentWorkspaceArchive, record recovery.Record, url string) {
	t.Helper()
	if err := r.importArchivedForkSource(t.Context(), run, reader, archive); err != nil {
		t.Fatal(err)
	}
	_, err := r.worktrees.WithExistingMirror(t.Context(), url, func(repository string) error {
		recoveryCLIGit(t, repository, "update-ref", record.Ref, record.BaseSHA, record.SnapshotSHA)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := run.Seq()
	if err := r.retire(run); err == nil || run.Seq() != before {
		t.Fatal("changed source ref deleted or acknowledged", err)
	}
	_, err = r.worktrees.WithExistingMirror(t.Context(), url, func(repository string) error {
		if got := recoveryCLIGit(t, repository, "rev-parse", record.Ref); got != record.BaseSHA {
			t.Fatal("foreign source ref changed", got)
		}
		recoveryCLIGit(t, repository, "update-ref", record.Ref, record.SnapshotSHA, record.BaseSHA)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.retire(run); err != nil || run.Seq() != before {
		t.Fatal("exact source retry changed archive cycle", err)
	}
	verifyForkSourcePin(t, r.worktrees, url, record, false)
}

func verifyRetainedForkSource(t *testing.T, r parentArchiveRestorer, run *journal.Run, reader *journal.Reader, archive runner.ParentWorkspaceArchive, record recovery.Record, url string) {
	t.Helper()
	if err := r.importArchivedForkSource(t.Context(), run, reader, archive); err != nil {
		t.Fatal(err)
	}
	_, err := r.worktrees.WithExistingMirror(t.Context(), url, func(repository string) error {
		_, _, err := recovery.PublishToInventoryWithEviction(t.Context(), repository, filepath.Join(r.layout.Root, "recovery"), []string{r.worktrees.Root}, record, 100, 16<<20, nil)
		return err
	})
	if err != nil {
		t.Fatal("shared source retention", err)
	}
	if err := r.retire(run); err != nil {
		t.Fatal("source retention transfer", err)
	}
	verifyForkSourcePin(t, r.worktrees, url, record, true)
}
