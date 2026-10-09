//go:build integration

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationHostForkArchivesAndRestoresWithoutWorkerReturn(t *testing.T) {
	testdep.Require(t, "git")
	for _, checkpoint := range []string{"ready", "active", "held", "uncreated"} {
		t.Run(checkpoint, func(t *testing.T) { verifyHostForkArchiveRecovery(t, checkpoint) })
	}
}

func verifyHostForkArchiveRecovery(t *testing.T, checkpoint string) {
	t.Helper()
	f := containedParentFixture(t)
	run, env := configuredChildStage(t, f)
	env.RepoRef = f.applied.Gaggles[0].Spec.Project
	repo := env.Workspace
	recoveryCLIGit(t, repo, "init", "--initial-branch=main")
	writeFileContent(t, filepath.Join(repo, "source.txt"), "base\n")
	recoveryCLIGit(t, repo, "add", ".")
	recoveryCLIGit(t, repo, "commit", "-m", "base")
	base := recoveryCLIGit(t, repo, "rev-parse", "HEAD")
	layout, err := instance.EffectiveWorkcopiesLayout(f.layout.ForGaggle(env.Gaggle), f.cfg, &f.applied.Gaggles[0])
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	url, err := childRepoCloneURL(env.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.WithRecoveryMirror(t.Context(), url, func(mirror string) error { recoveryCLIGit(t, mirror, "fetch", repo, "main"); return nil }); err != nil {
		t.Fatal(err)
	}
	source, err := manager.CreateChildFromSnapshot(t.Context(), worktree.ChildOptions{RepoURL: url, RunID: env.RunID + "-source", OwnerRunID: env.RunID, Gaggle: env.Gaggle, SnapshotSHA: base})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{{Branch: 1, Name: "a"}, {Branch: 2, Name: "b"}}}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	started := events[len(events)-1]
	backend := parallelworkspace.Service{Worktrees: manager, CloneURL: childRepoCloneURL, Policy: func(workspace string) (recovery.SnapshotPolicy, error) {
		return childSnapshotPolicy(workspace, layout.Root, f.cfg)
	}}
	seed, err := backend.Prepare(t.Context(), run, spec.Request{RunID: env.RunID, Gaggle: env.Gaggle, Parallel: "fan", Sequence: started.Seq, At: started.Time, Repository: env.RepoRef, Workspace: source.Path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	options := worktree.ParallelForkOptions{RepoURL: url, OwnerRunID: env.RunID, Gaggle: env.Gaggle, ParallelSequence: started.Seq, Branch: 1, SnapshotSHA: seed.SnapshotSHA}
	custody, err := worktree.ParallelForkCustody(options)
	if err != nil {
		t.Fatal(err)
	}
	siblingOptions := options
	siblingOptions.Branch = 2
	sibling, err := worktree.ParallelForkCustody(siblingOptions)
	if err != nil {
		t.Fatal(err)
	}
	plan := runner.ParentForkPlan{Version: 1, RunID: env.RunID, Gaggle: env.Gaggle, Parallel: "fan", Sequence: started.Seq, Source: seed, Workspaces: []worktree.StageCustody{custody, sibling}}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := run.RecordArtifactBoundedWithIntegrity("fork-plan.json", encoded, apiv1.IntegrityTrusted, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Parallel: "fan", Runner: map[string]any{"kind": runner.ParentForkPlannedKind, "plan": ref}}); err != nil {
		t.Fatal(err)
	}
	var checkout *worktree.Worktree
	head, index := seed.SnapshotSHA, ""
	if checkpoint != "uncreated" {
		checkout, err = manager.CreateParallelFromSnapshot(t.Context(), options)
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint != "active" {
			if _, err := checkout.HoldForChild(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if checkpoint == "ready" {
			if err := runner.RecordParentForkReady(run, plan.Sequence, ref, 1, custody); err != nil {
				t.Fatal(err)
			}
		}
		// No worker was launched: preserve ordinary edits even without ready.
		writeFileContent(t, filepath.Join(checkout.Path, "source.txt"), "ordinary committed\n")
		recoveryCLIGit(t, checkout.Path, "add", "source.txt")
		recoveryCLIGit(t, checkout.Path, "commit", "-m", "ordinary branch work")
		head = recoveryCLIGit(t, checkout.Path, "rev-parse", "HEAD")
		writeFileContent(t, filepath.Join(checkout.Path, "source.txt"), "ordinary staged\n")
		recoveryCLIGit(t, checkout.Path, "add", "source.txt")
		writeFileContent(t, filepath.Join(checkout.Path, "source.txt"), "ordinary working\n")
		if err := os.WriteFile(filepath.Join(checkout.Path, "untracked.bin"), []byte{0, 255, 17}, 0600); err != nil {
			t.Fatal(err)
		}
		index = recoveryCLIGit(t, checkout.Path, "write-tree")
	}
	// The second reserved branch has never been created in any checkpoint.
	// Recovery must use the durable snapshot even after the root changes.
	writeFileContent(t, filepath.Join(source.Path, "source.txt"), "later root edits\n")
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	service := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, service) })
	if err := service.enableChildWorkflows(queue, f.applied); err != nil {
		t.Fatal(err)
	}
	restorer := parentArchiveRestorer{layout: layout, config: f.cfg, worktrees: manager, cloneURL: childRepoCloneURL}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted)}); err != nil {
		t.Fatal(err)
	}
	// A startup sweep cannot borrow the live owner's journal lease.
	beforeRecovery := run.Seq()
	if err := releaseTerminalParentArchives(layout, manager, env.RunID); !errors.Is(err, journal.ErrRecoveryBusy) {
		t.Fatal("fork cleanup borrowed live journal", err)
	}
	if run.Seq() != beforeRecovery {
		t.Fatal("busy cleanup mutated the journal")
	}
	if checkpoint == "ready" {
		interruptForkSourceRelease(t, restorer, run, reader)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := restorer.retryRetirement(reader, nil); err != nil {
		t.Fatal("startup host fork retirement", err)
	}
	run, _, err = journal.TryRecover(reader.Dir())
	if err != nil {
		t.Fatal("retirement leaked journal lease", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	checkout, err = manager.AdoptHeldStage(t.Context(), url, custody)
	if err != nil {
		t.Fatal("recovered fork not held", err)
	}
	second, err := manager.AdoptHeldStage(t.Context(), url, sibling)
	if err != nil {
		t.Fatal("uncreated sibling not recovered", err)
	}
	if got := readFileContent(t, filepath.Join(second.Path, "source.txt")); got != "base\n" {
		t.Fatal("sibling recaptured later root edits", got)
	}
	if pending, err := runner.PendingParentForks(reader); err != nil || len(pending) != 0 {
		t.Fatal("recovered plan still pending", pending, err)
	}
	candidates, err := runner.ParentRetirementCandidates(reader)
	if err != nil || len(candidates) != 2 || candidates[0].Workspace.Fork == nil || candidates[0].RetirementSeq == 0 {
		t.Fatal("fork archive missing", candidates, err)
	}
	retired, err := runner.RetiredParentForks(reader)
	if err != nil || len(retired) != 1 || !retired[0].ReleaseRecorded {
		t.Fatal("source release not acknowledged", retired, err)
	}
	snapshot, err := parallelworkspace.ReadSource(reader, seed, plan.Parallel, plan.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	verifyForkSourcePin(t, manager, url, snapshot.Record, false)
	candidate := candidates[0]
	for _, mode := range []string{"foreign-plan", "mixed-worker", "fake-origin"} {
		wrong := candidate.Workspace
		fork := *wrong.Fork
		wrong.Fork = &fork
		switch mode {
		case "foreign-plan":
			fork.Plan.Digest = journal.Digest([]byte("foreign plan"))
		case "mixed-worker":
			wrong.ContractDigest = journal.Digest([]byte("worker contract"))
		case "fake-origin":
			wrong.Custody.Origin = &apiv1.ChildWorkflowOrigin{StageOccurrence: "foreign"}
		}
		if _, _, err := restorer.authorize(t.Context(), reader, wrong); err == nil {
			t.Fatal("invalid fork archive authority admitted", mode)
		}
	}
	before := run.Seq()
	if err := restorer.retire(run); err != nil || run.Seq() != before {
		t.Fatal("retirement replay", err)
	}
	if checkpoint == "held" {
		verifyChangedForkSourceRef(t, restorer, run, reader, candidate.Workspace, snapshot.Record, url)
	}
	guard, err := recoveryCleanupOption(layout, f.cfg, manager.Root, childRepoCloneURL, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	guard(manager)
	if err := restorer.releaseArchives(reader, candidates); err != nil {
		t.Fatal("fork hold release", err)
	}
	if err := checkout.Remove(t.Context(), worktree.RemoveOptions{}); err != nil {
		t.Fatal("fork cleanup", err)
	}
	if _, err := os.Stat(checkout.Path); !os.IsNotExist(err) {
		t.Fatal("fork survived cleanup", err)
	}
	rec, err := runner.OwnedBranchRecorder(run, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := restorer.restore(t.Context(), run, candidate.Workspace, candidate.RetirementSeq); err == nil {
		t.Fatal("root recorder restored a branch fork")
	}
	if err := restorer.restore(t.Context(), rec, candidate.Workspace, candidate.RetirementSeq); err != nil {
		t.Fatal("fork restoration", err)
	}
	if recoveryCLIGit(t, checkout.Path, "rev-parse", "HEAD") != head || index != "" && recoveryCLIGit(t, checkout.Path, "write-tree") != index {
		t.Fatal("restoration changed HEAD or index")
	}
	expected := "ordinary working\n"
	if checkpoint == "uncreated" {
		expected = "base\n"
	}
	if got := readFileContent(t, filepath.Join(checkout.Path, "source.txt")); got != expected {
		t.Fatal("working file lost", got)
	}
	if data, err := os.ReadFile(filepath.Join(checkout.Path, "untracked.bin")); checkpoint != "uncreated" && (err != nil || string(data) != string([]byte{0, 255, 17})) {
		t.Fatal("binary lost", data, err)
	}
	if _, err := manager.AdoptHeldStage(t.Context(), url, custody); err != nil {
		t.Fatal("restoration lost exact hold", err)
	}
	verifyForkSourcePin(t, manager, url, snapshot.Record, true)
	if _, err := runner.RetiredParentForks(reader); !errors.Is(err, runner.ErrParentReturnPending) {
		t.Fatal("restored branch retained release authority", err)
	}
	writeFileContent(t, filepath.Join(checkout.Path, "source.txt"), "new cycle\n")
	if err := restorer.retire(run); err != nil {
		t.Fatal("fork retirement after restoration", err)
	}
	verifyForkSourcePin(t, manager, url, snapshot.Record, false)
	next, err := runner.ParentRetirementCandidates(reader)
	if err != nil || len(next) != 2 || next[0].RetirementSeq <= candidate.RetirementSeq || next[0].Workspace.Archive == candidate.Workspace.Archive {
		t.Fatal("restored fork reused obsolete archive", next, err)
	}
	if checkpoint == "held" {
		verifyRetainedForkSource(t, restorer, run, reader, next[0].Workspace, snapshot.Record, url)
	}
}

func verifyForkSourcePin(t *testing.T, manager *worktree.Manager, url string, record recovery.Record, want bool) {
	t.Helper()
	found, err := manager.WithExistingMirror(t.Context(), url, func(repository string) error {
		if got := recovery.HasSnapshotRef(t.Context(), repository, record); got != want {
			t.Errorf("source pin exists=%v, want %v", got, want)
		}
		return nil
	})
	if err != nil || !found {
		t.Fatal("source mirror unavailable", err)
	}
}
