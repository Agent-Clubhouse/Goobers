//go:build integration

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

type parentArchiveLogFunc func(journal.Event) error

func (f parentArchiveLogFunc) Append(event journal.Event) error { return f(event) }

func verifyLatestParentCleanupArchive(t *testing.T, reader *journal.Reader, repo, key string, custody worktree.StageCustody, restorer parentArchiveRestorer, checkout *worktree.Worktree) {
	t.Helper()
	target := worktree.CleanupTarget{Path: repo, WorktreeID: custody.WorkspaceID, OwnerRunID: custody.OwnerRunID, RepositoryDigest: custody.RepositoryDigest, StartRef: custody.StartRef, BaseRef: custody.StartRef, RetainOnCleanup: true}
	policy, err := parentCleanupPolicy(t.Context(), reader, target, key)
	if err != nil || policy == nil {
		t.Fatal("cleanup did not resolve the verified parent's policy", err)
	}
	wrong := target
	wrong.RepositoryDigest = journal.Digest([]byte("different repository"))
	if _, err := parentCleanupPolicy(t.Context(), reader, wrong, key); err == nil {
		t.Fatal("cleanup accepted a substituted repository")
	}
	// Ordinary stages can advance the branch after the pod's last return.
	// Archive the live checkout, not that older retained output artifact.
	recoveryCLIGit(t, repo, "commit", "-m", "ordinary stage committed after worker")
	head := recoveryCLIGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("ordinary staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, repo, "add", "source.txt")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("ordinary dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	root, err := prepareRecoveryInventory(restorer.layout.Root)
	if err != nil {
		t.Fatal(err)
	}
	request := recovery.RetentionRequest{Repository: repo, RepositoryKey: key, RunID: id.RunID, BaseRef: custody.StartRef, IdentityTime: id.StartedAt, RetainUntil: time.Now().Add(24 * time.Hour), InventoryRoot: root, CleanupRoots: []string{repo}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20, ParentPolicy: policy}
	acknowledged := false
	record, path, err := recovery.Retain(t.Context(), request, parentArchiveLogFunc(func(event journal.Event) error {
		acknowledged = event.RunID == id.RunID && event.Runner["recoveryCapture"] == true
		return nil
	}))
	if err != nil || path == "" || !acknowledged {
		t.Fatal("parent cleanup archive not acknowledged", err)
	}
	state, err := recovery.ReadRetainedParentState(t.Context(), repo, record)
	if err != nil || state.HeadSHA != head {
		t.Fatal("archive lost subsequent ordinary commit", state, err)
	}
	if recoveryCLIGit(t, repo, "show", state.IndexSHA+":source.txt") != "ordinary staged" || recoveryCLIGit(t, repo, "show", record.SnapshotSHA+":source.txt") != "ordinary dirty" {
		t.Fatal("archive reused stale worker output")
	}
	verifyParentArchiveDaemonRestoration(t, reader, restorer, checkout, record, state)

}

func verifyParentArchiveDaemonRestoration(t *testing.T, reader *journal.Reader, restorer parentArchiveRestorer, checkout *worktree.Worktree, record recovery.Record, state recovery.RetainedParentState) {
	t.Helper()
	writer, _, err := journal.TryRecover(reader.Dir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := writer.RecordArtifactWithIntegrity("parent-archive-record.json", data, apiv1.IntegrityTrusted)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := recoveryCleanupOption(restorer.layout, restorer.config, restorer.worktrees.Root, restorer.cloneURL, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	guard(restorer.worktrees)
	for _, mode := range []string{"survived", "removed", "partial", "released"} {
		if err := runner.RecordParentArchiveRetirement(writer, checkout.Branch, ref); err != nil {
			t.Fatal(mode, err)
		}
		archive, seq := latestParentArchive(t, reader)
		if mode == "survived" {
			verifyUnavailableParentArchive(t, reader, restorer, writer, archive, seq, record)
			verifyRetiredParentCleanupRefusals(t, reader, restorer, writer, archive, checkout, record)
		}
		wrong := archive
		wrong.ContractDigest = journal.Digest([]byte("foreign"))
		if err := restorer.restore(t.Context(), writer, wrong, seq); err == nil {
			t.Fatal("foreign archive receipt admitted")
		}
		if mode == "released" {
			candidates, err := runner.ParentRetirementCandidates(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := restorer.releaseArchives(reader, candidates); err != nil {
				t.Fatal("release before interrupted cleanup", err)
			}
		}
		if mode != "survived" && mode != "released" {
			if err := checkout.ReleaseChildHold(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := checkout.Remove(t.Context(), worktree.RemoveOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(checkout.Path); !os.IsNotExist(err) {
				t.Fatal("source checkout not removed", err)
			}
		}
		if mode == "partial" {
			project, err := recoveryConfiguredProject(restorer.config, record.RepositoryKey)
			if err != nil {
				t.Fatal(err)
			}
			url, err := restorer.cloneURL(project)
			if err != nil {
				t.Fatal(err)
			}
			c := archive.Custody.Workspace
			id, err := reader.Identity()
			if err != nil {
				t.Fatal(err)
			}
			checkout, err = restorer.worktrees.CreateParentRestore(t.Context(), worktree.ParentRestoreOptions{RepoURL: url, RunID: c.WorkspaceID, OwnerRunID: c.OwnerRunID, Gaggle: id.Gaggle, Branch: c.Branch, BaseRef: record.BaseSHA, HeadSHA: state.HeadSHA, StartRef: c.StartRef})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := checkout.HoldForChild(t.Context()); err != nil {
				t.Fatal(err)
			}
			plan, err := recovery.PlanRetainedParentRestore(t.Context(), checkout.Path, record, "partial-restore", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if err := recordParentRestorePlan(writer, parentRestorePlan{Version: 1, RetirementSeq: seq, Archive: archive.Archive, Plan: plan}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(checkout.Path, "source.txt"), []byte("ordinary dirty\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err := restorer.restore(t.Context(), writer, archive, seq); err != nil {
				t.Fatal(mode, err)
			}
		}
		if got := recoveryCLIGit(t, checkout.Path, "rev-parse", "HEAD"); got != state.HeadSHA {
			t.Fatal(mode, "original history lost", got)
		}
		if got := recoveryCLIGit(t, checkout.Path, "show", ":source.txt"); got != "ordinary staged" {
			t.Fatal(mode, "index lost", got)
		}
		if got, err := os.ReadFile(filepath.Join(checkout.Path, "source.txt")); err != nil || string(got) != "ordinary dirty\n" {
			t.Fatal(mode, "working state lost", string(got), err)
		}
	}
	verifyAutomaticParentRetirement(t, reader, restorer, writer, checkout)
}

func verifyAutomaticParentRetirement(t *testing.T, reader *journal.Reader, restorer parentArchiveRestorer, writer *journal.Run, checkout *worktree.Worktree) {
	t.Helper()
	foreign := restorer
	foreign.layout = restorer.layout.ForGaggle("foreign")
	if err := foreign.retire(writer); err == nil {
		t.Fatal("retirement crossed gaggle journal scope")
	}
	service, ok := stageGrantMinterFor(restorer.layout.Root).(*daemonCredentialService)
	if !ok {
		t.Fatal("fixture lacks child custody service")
	}
	unregisterDaemonStageGrants(restorer.layout.Root, service)
	err := restorer.retire(writer)
	registerStageGrantMinter(restorer.layout.Root, service)
	if err == nil {
		t.Fatal("retirement ignored unavailable child custody")
	}
	if _, err := checkout.HeldCleanupTarget(t.Context()); err != nil {
		t.Fatal("failed retirement released hold", err)
	}
	if err := os.WriteFile(filepath.Join(checkout.Path, "source.txt"), []byte("terminal dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restorer.retire(writer); err != nil {
		t.Fatal("automatic parent retirement", err)
	}
	archive, _ := latestParentArchive(t, reader)
	data, err := reader.ArtifactBytesBounded(archive.Archive, 16384)
	if err != nil {
		t.Fatal(err)
	}
	var record recovery.Record
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	at, terminal, err := recoveryCaptureWindow(t.Context(), reader, id.StartedAt)
	if err != nil || !terminal || !record.CreatedAt.Equal(at) {
		t.Fatal("retirement missed stable terminal anchor", record.CreatedAt, at, err)
	}
	if got := recoveryCLIGit(t, checkout.Path, "show", record.SnapshotSHA+":source.txt"); got != "terminal dirty" {
		t.Fatal("retirement reused stale archive", got)
	}
	before, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := restorer.retire(writer); err != nil {
		t.Fatal("retirement replay", err)
	}
	after, err := reader.Events()
	if err != nil || len(after) != len(before) {
		t.Fatal("retirement replay wrote duplicate intent", err)
	}
	if _, err := checkout.HeldCleanupTarget(t.Context()); err != nil {
		t.Fatal("retirement released hold before finalizer", err)
	}
	verifyAutomaticParentRelease(t, reader, restorer, writer, checkout)
}

func verifyAutomaticParentRelease(t *testing.T, reader *journal.Reader, restorer parentArchiveRestorer, writer *journal.Run, checkout *worktree.Worktree) {
	t.Helper()
	if err := instance.WriteConfig(restorer.layout.ConfigFile(), restorer.config); err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	claimsReleased := 0
	finalize := func() error {
		return finalizeTerminalRunWithClaimRelease(restorer.layout, nil, restorer.worktrees, id.RunID, func(instance.Layout, *journal.InstanceLog, string) error {
			claimsReleased++
			return nil
		})
	}
	if err := os.WriteFile(filepath.Join(checkout.Path, "source.txt"), []byte("unarchived edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := finalize(); err == nil || claimsReleased != 1 {
		t.Fatal("failed release did not preserve cleanup error and release claims", claimsReleased, err)
	}
	if _, err := checkout.HeldCleanupTarget(t.Context()); err != nil {
		t.Fatal("failed verification changed held custody", err)
	}
	if err := os.WriteFile(filepath.Join(checkout.Path, "source.txt"), []byte("terminal dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := finalize(); err != nil {
		t.Fatal("automatic terminal parent cleanup", err)
	}
	if _, err := os.Lstat(checkout.Path); !os.IsNotExist(err) {
		t.Fatal("automatic finalizer retained archived checkout", err)
	}
	if err := finalize(); err != nil || claimsReleased != 3 {
		t.Fatal("terminal cleanup retry", claimsReleased, err)
	}
	archive, seq := latestParentArchive(t, reader)
	if err := restorer.restore(t.Context(), writer, archive, seq); err != nil {
		t.Fatal("automatic cleanup lost resumable archive", err)
	}
	if got := recoveryCLIGit(t, checkout.Path, "show", ":source.txt"); got != "ordinary staged" {
		t.Fatal("automatic cleanup lost staging", got)
	}
	if got, err := os.ReadFile(filepath.Join(checkout.Path, "source.txt")); err != nil || string(got) != "terminal dirty\n" {
		t.Fatal("automatic cleanup lost latest working state", string(got), err)
	}
	verifyParentRetirementStartup(t, reader, restorer, writer, checkout)
}

func verifyParentRetirementStartup(t *testing.T, reader *journal.Reader, restorer parentArchiveRestorer, writer *journal.Run, checkout *worktree.Worktree) {
	t.Helper()
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout.Path, "source.txt"), []byte("startup dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	claimsReleased := 0
	finalize := func() error {
		return finalizeTerminalRunWithClaimRelease(restorer.layout, nil, restorer.worktrees, id.RunID, func(instance.Layout, *journal.InstanceLog, string) error {
			claimsReleased++
			return nil
		})
	}
	if err := finalize(); !errors.Is(err, journal.ErrRecoveryBusy) || claimsReleased != 1 {
		t.Fatal("startup cleanup borrowed live writer or skipped claim release", claimsReleased, err)
	}
	if _, err := checkout.HeldCleanupTarget(t.Context()); err != nil {
		t.Fatal("busy retirement released custody", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := finalize(); err != nil || claimsReleased != 2 {
		t.Fatal("startup did not complete missing retirement", claimsReleased, err)
	}
	if _, err := os.Lstat(checkout.Path); !os.IsNotExist(err) {
		t.Fatal("startup retirement retained original checkout", err)
	}
	recovered, _, err := journal.TryRecover(reader.Dir())
	if err != nil {
		t.Fatal("startup cleanup leaked journal lease", err)
	}
	defer func() { _ = recovered.Close() }()
	archive, seq := latestParentArchive(t, reader)
	if err := restorer.restore(t.Context(), recovered, archive, seq); err != nil {
		t.Fatal("startup archive could not restore", err)
	}
	if got, err := os.ReadFile(filepath.Join(checkout.Path, "source.txt")); err != nil || string(got) != "startup dirty\n" {
		t.Fatal("startup archive lost latest working state", string(got), err)
	}
}

func verifyRetiredParentCleanupRefusals(t *testing.T, reader *journal.Reader, restorer parentArchiveRestorer, writer *journal.Run, archive runner.ParentWorkspaceArchive, checkout *worktree.Worktree, record recovery.Record) {
	t.Helper()
	c := archive.Custody.Workspace
	target := worktree.CleanupTarget{Path: checkout.Path, WorktreeID: c.WorkspaceID, OwnerRunID: c.OwnerRunID, RepositoryDigest: c.RepositoryDigest, StartRef: c.StartRef, BaseRef: record.BaseSHA, RetainOnCleanup: true}
	check := func() error {
		handled, err := retiredParentCleanup(t.Context(), restorer.layout, restorer.config, restorer.worktrees, reader, record.RepositoryKey, target)
		if !handled {
			t.Fatal("retired checkout fell back to ordinary capture")
		}
		return err
	}
	if err := check(); err == nil {
		t.Fatal("cleanup accepted a nonterminal owner")
	}
	if err := writer.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout.Path, "source.txt"), []byte("intervening edit\\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("cleanup discarded changes after archive capture")
	}
	if err := os.WriteFile(filepath.Join(checkout.Path, "source.txt"), []byte("ordinary dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal("unchanged retired checkout rejected", err)
	}
}

func verifyUnavailableParentArchive(t *testing.T, reader *journal.Reader, restorer parentArchiveRestorer, writer *journal.Run, archive runner.ParentWorkspaceArchive, seq uint64, record recovery.Record) {
	t.Helper()
	path, err := parentArchiveInventoryPath(t.Context(), restorer.layout, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".unavailable"); err != nil {
		t.Fatal(err)
	}
	err = restorer.restore(t.Context(), writer, archive, seq)
	if restoreErr := os.Rename(path+".unavailable", path); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("missing archive acknowledged restoration")
	}
	if pending, err := runner.ParentArchiveRestorationPending(writer, archive, seq); err != nil || !pending {
		t.Fatal("failed import consumed retirement", pending, err)
	}
	if _, found, err := readParentRestorePlan(reader, 0, archive.Archive, seq); err != nil || found {
		t.Fatal("failed inventory check prepared file mutation", found, err)
	}
}

func latestParentArchive(t *testing.T, reader *journal.Reader) (runner.ParentWorkspaceArchive, uint64) {
	t.Helper()
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Runner["kind"] != runner.ParentContributionRetiredKind {
			continue
		}
		var archive runner.ParentWorkspaceArchive
		data, err := json.Marshal(events[i].Runner["archive"])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &archive); err != nil {
			t.Fatal(err)
		}
		return archive, events[i].Seq
	}
	t.Fatal("retirement missing")
	return runner.ParentWorkspaceArchive{}, 0
}
