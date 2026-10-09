//go:build integration

package runner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Exercise source capture, whole-fanout planning, physical workspaces, ordinary
// stage selection and a reopened journal. Public parallel admission/fan-in is
// intentionally not part of this fixture, which uses the serial parent machine.
func TestIntegrationParallelForkPlanPreservesSourceAndBranchEdits(t *testing.T) {
	testdep.Require(t, "git")
	r, run, frame, _, _ := prepareChildWaitRuntime(t)
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	source, err := r.createStageWorkspace(t.Context(), frame.in, frame.t.Name, apiv1.WorkspaceRepo, false, "")
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{RunID: frame.in.RunID, Gaggle: frame.in.Gaggle, WorkflowID: frame.in.Machine.Def.Name, Attempt: 1, ChildWorkflowOrigin: frame.childOrigin}
	if err := holdContainedParentWorkspace(t.Context(), frame, source, env); err != nil {
		t.Fatal(err)
	}
	output, err := run.RecordArtifact("verified-parent-output.json", []byte("earlier worker output"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordParentContribution(run, env, journal.Digest([]byte("contract")), output); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: frame.t.Name, Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatal(err)
	}
	// Later ordinary-stage edits must be included, not the stale worker output.
	childWorkspaceWrite(t, source.path, "main.txt", []byte("staged source\n"))
	runGit(t, source.path, "add", "main.txt")
	childWorkspaceWrite(t, source.path, "main.txt", []byte("working source\n"))
	childWorkspaceWrite(t, source.path, "ordinary.bin", []byte{0, 255, 1})
	sourceHead, sourceIndex := gitOutput(t, source.path, "rev-parse", "HEAD"), gitOutput(t, source.path, "write-tree")
	captureCalls := 0
	r.cfg.PrepareParentForkSource = func(ctx context.Context, rec OwnedJournalRecorder, request spec.Request, previous *spec.Source) (spec.Source, error) {
		service := parallelworkspace.Service{Worktrees: r.cfg.Worktrees, CloneURL: r.cfg.RepoCloneURL, Policy: func(string) (recovery.SnapshotPolicy, error) { captureCalls++; return recovery.SnapshotPolicy{}, nil }}
		return service.Prepare(ctx, rec, request, previous)
	}
	parallel := apiv1.Parallel{Name: "fan", MaxConcurrentBranches: 1, Branches: []apiv1.Branch{{Name: "a", Start: frame.t.Name}, {Name: "b", Start: frame.t.Name}}}
	par := newParallelExec(parallel)
	if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: parallel.Name, Completeness: par.completeness()}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	readEvents := func() []journal.Event {
		t.Helper()
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	forks, err := r.prepareParallelForks(t.Context(), run, frame.in, parallel, "", readEvents())
	if err != nil {
		t.Fatal(err)
	}
	if len(forks) != 2 || captureCalls != 1 || forks[0].Branch == forks[1].Branch {
		t.Fatal("fork identity/capture", forks, captureCalls)
	}
	firstIn := parallelBranchInput(frame.in, parallelRuntime{forks: forks}, nil, 1, "")
	first, err := r.createStageWorkspace(t.Context(), firstIn, "ordinary", apiv1.WorkspaceRepo, false, firstIn.WorkspaceBranch)
	if err != nil {
		t.Fatal(err)
	}
	childWorkspaceRead(t, first.path, "main.txt", []byte("working source\n"))
	childWorkspaceRead(t, first.path, "ordinary.bin", []byte{0, 255, 1})
	childWorkspaceWrite(t, first.path, "main.txt", []byte("branch staged\n"))
	runGit(t, first.path, "add", "main.txt")
	childWorkspaceWrite(t, first.path, "main.txt", []byte("branch working\n"))
	firstIndex := gitOutput(t, first.path, "write-tree")
	if err := first.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidates, err := ParentRetirementCandidates(reader)
	if err != nil || len(candidates) != 3 {
		t.Fatal("host forks lost archive ownership", candidates, err)
	}
	forksToArchive := 0
	for _, candidate := range candidates {
		if candidate.Workspace.Fork != nil {
			forksToArchive++
			if candidate.Workspace.Custody.Origin != nil || candidate.Workspace.ContractDigest != "" || candidate.RetirementSeq != 0 {
				t.Fatal("host fork impersonated a worker return", candidate)
			}
		}
	}
	if forksToArchive != 2 {
		t.Fatal("missing fork source authority", forksToArchive)
	}
	dir := run.Dir()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	r.cfg.Worktrees, err = worktree.NewManager(r.cfg.Worktrees.Root, worktree.WithRemoteGitGate(func(context.Context, string) error { return errors.New("fork replay must not fetch") }))
	if err != nil {
		t.Fatal(err)
	}
	forks, err = r.prepareParallelForks(t.Context(), reopened, frame.in, parallel, "", readEvents())
	if err != nil {
		t.Fatal(err)
	}
	if captureCalls != 1 {
		t.Fatal("restart recaptured source")
	}
	firstIn = parallelBranchInput(frame.in, parallelRuntime{forks: forks}, nil, 1, "")
	restored, err := r.createStageWorkspace(t.Context(), firstIn, "next", apiv1.WorkspaceRepo, false, firstIn.WorkspaceBranch)
	if err != nil {
		t.Fatal(err)
	}
	if restored.path != first.path || gitOutput(t, restored.path, "write-tree") != firstIndex {
		t.Fatal("restart reset branch index")
	}
	childWorkspaceRead(t, restored.path, "main.txt", []byte("branch working\n"))
	secondIn := parallelBranchInput(frame.in, parallelRuntime{forks: forks}, nil, 2, "")
	sibling, err := r.createStageWorkspace(t.Context(), secondIn, "ordinary", apiv1.WorkspaceRepo, false, secondIn.WorkspaceBranch)
	if err != nil {
		t.Fatal(err)
	}
	childWorkspaceRead(t, sibling.path, "main.txt", []byte("working source\n"))
	if filepath.Clean(source.path) == filepath.Clean(first.path) || gitOutput(t, source.path, "rev-parse", "HEAD") != sourceHead || gitOutput(t, source.path, "write-tree") != sourceIndex {
		t.Fatal("source custody changed")
	}
	childWorkspaceRead(t, source.path, "main.txt", []byte("working source\n"))
}

func TestIntegrationParallelForkRecoversHoldBeforeReadyWithoutPriorParent(t *testing.T) {
	testdep.Require(t, "git")
	r, run, frame, _, _ := prepareChildWaitRuntime(t)
	captures := 0
	r.cfg.PrepareParentForkSource = func(ctx context.Context, rec OwnedJournalRecorder, request spec.Request, previous *spec.Source) (spec.Source, error) {
		service := parallelworkspace.Service{Worktrees: r.cfg.Worktrees, CloneURL: r.cfg.RepoCloneURL, Policy: func(string) (recovery.SnapshotPolicy, error) { captures++; return recovery.SnapshotPolicy{}, nil }}
		return service.Prepare(ctx, rec, request, previous)
	}
	parallel := apiv1.Parallel{Name: "fan", Branches: []apiv1.Branch{{Name: "a", Start: frame.t.Name}, {Name: "b", Start: frame.t.Name}}}
	par := newParallelExec(parallel)
	if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: parallel.Name, Completeness: par.completeness()}); err != nil {
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
	started, err := parallelForkBoundary(events, parallel)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.captureParallelForkSource(t.Context(), run, frame.in, started, "")
	if err != nil {
		t.Fatal("fork without prior contained stage", err)
	}
	url, err := r.cfg.RepoCloneURL(frame.in.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	plan.Workspaces, err = parallelForkOwners(url, frame.in, plan, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordParallelForkPlan(run, plan); err != nil {
		t.Fatal(err)
	}
	workspace, err := r.cfg.Worktrees.CreateParallelFromSnapshot(t.Context(), worktree.ParallelForkOptions{RepoURL: url, OwnerRunID: frame.in.RunID, Gaggle: frame.in.Gaggle, ParallelSequence: plan.Sequence, Branch: 1, SnapshotSHA: plan.Source.SnapshotSHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.HoldForChild(t.Context()); err != nil {
		t.Fatal(err)
	}
	childWorkspaceWrite(t, workspace.Path, "preserved.bin", []byte{0, 17, 255})
	// Crash after checkout hold but before readiness, with branch 2 never created.
	dir := run.Dir()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	r.cfg.Worktrees, err = worktree.NewManager(r.cfg.Worktrees.Root, worktree.WithRemoteGitGate(func(context.Context, string) error { return errors.New("unexpected remote acquisition") }))
	if err != nil {
		t.Fatal(err)
	}
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	forks, err := r.prepareParallelForks(t.Context(), reopened, frame.in, parallel, "", events)
	if err != nil {
		t.Fatal(err)
	}
	if len(forks) != 2 || captures != 1 {
		t.Fatal("recovery replaced source", len(forks), captures)
	}
	childWorkspaceRead(t, workspace.Path, "preserved.bin", []byte{0, 17, 255})
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	states, err := readParentForkStates(reader, events)
	if err != nil || len(states[plan.Sequence].ready) != 2 {
		t.Fatal("recovery did not acknowledge both forks", err)
	}
	// A replay for a different repository must not reuse an unrelated source.
	foreign := frame.in
	foreign.RepoRef.Name = "other"
	if _, err := r.prepareParallelForks(t.Context(), reopened, foreign, parallel, "", events); err == nil {
		t.Fatal("changed source repository accepted")
	}
	childWorkspaceRead(t, workspace.Path, "preserved.bin", []byte{0, 17, 255})
}
