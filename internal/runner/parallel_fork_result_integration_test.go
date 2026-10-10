//go:build integration

package runner

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParallelForkResultsFreezeBeforeSettlement(t *testing.T) {
	testdep.Require(t, "git")
	r, run, frame, _, _ := prepareChildWaitRuntime(t)
	service := parallelworkspace.Service{Worktrees: r.cfg.Worktrees, CloneURL: r.cfg.RepoCloneURL, Policy: func(string) (recovery.SnapshotPolicy, error) {
		return recovery.SnapshotPolicy{ExcludedPaths: []string{"private.key"}}, nil
	}}
	r.cfg.PrepareParentForkSource = func(ctx context.Context, rec OwnedJournalRecorder, request spec.Request, previous *spec.Source) (spec.Source, error) {
		return service.Prepare(ctx, rec, request, previous)
	}
	captures := 0
	r.cfg.PrepareParentForkResult = func(ctx context.Context, rec OwnedJournalRecorder, request spec.ResultRequest, previous *spec.Source) (spec.Source, error) {
		if previous == nil {
			captures++
		}
		return service.Result(ctx, rec, request, previous)
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
	forks, err := r.prepareParallelForks(t.Context(), run, frame.in, parallel, "", events)
	if err != nil {
		t.Fatal(err)
	}
	url, err := r.cfg.RepoCloneURL(frame.in.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := []*parallelBranchResult{{index: 0, status: journal.BranchSucceeded}, {index: 1, status: journal.BranchFailed, failed: true}}
	runtime := parallelRuntime{forks: forks}
	branchFailure, captureFailure := errors.New("branch execution failed"), errors.New("capture refused")
	capture := r.cfg.PrepareParentForkResult
	r.cfg.PrepareParentForkResult = func(context.Context, OwnedJournalRecorder, spec.ResultRequest, *spec.Source) (spec.Source, error) {
		return spec.Source{}, captureFailure
	}
	failed := *outcomes[0]
	failed.err = branchFailure
	if err := r.settleParallelRuntimeBranch(t.Context(), run, frame.in, par, runtime, failed); !errors.Is(err, branchFailure) || !errors.Is(err, captureFailure) {
		t.Fatal("workspace refusal hid branch failure", err)
	}
	r.cfg.PrepareParentForkResult = capture
	if err := r.verifyParallelForkResults(t.Context(), run, frame.in, parallel, runtime, outcomes); err == nil {
		t.Fatal("unrecorded results admitted")
	}
	var checkouts []*worktree.Worktree
	var heads, indexes []string
	for index, owner := range forks {
		checkout, err := r.cfg.Worktrees.AdoptHeldStage(t.Context(), url, owner)
		if err != nil {
			t.Fatal(err)
		}
		checkouts = append(checkouts, checkout)
		childWorkspaceWrite(t, checkout.Path, "committed.txt", []byte("branch commit\n"))
		childWorkspaceWrite(t, checkout.Path, "private.key", []byte("excluded committed material\n"))
		runGit(t, checkout.Path, "add", "committed.txt", "private.key")
		runGit(t, checkout.Path, "commit", "-m", "branch work")
		childWorkspaceWrite(t, checkout.Path, "main.txt", []byte("staged\n"))
		runGit(t, checkout.Path, "add", "main.txt")
		childWorkspaceWrite(t, checkout.Path, "main.txt", []byte("working\n"))
		childWorkspaceWrite(t, checkout.Path, "untracked.bin", []byte{0, 17, 255})
		heads = append(heads, gitOutput(t, checkout.Path, "rev-parse", "HEAD"))
		indexes = append(indexes, gitOutput(t, checkout.Path, "write-tree"))
		if err := r.settleParallelRuntimeBranch(t.Context(), run, frame.in, par, runtime, *outcomes[index]); err != nil {
			t.Fatal(err)
		}
	}
	if captures != 2 {
		t.Fatal("result capture count", captures)
	}
	for index, checkout := range checkouts {
		if gitOutput(t, checkout.Path, "rev-parse", "HEAD") != heads[index] || gitOutput(t, checkout.Path, "write-tree") != indexes[index] {
			t.Fatal("result capture changed Git state")
		}
		request, source, err := parallelForkResultRequest(reader, frame.in, parallel, *outcomes[index])
		if err != nil || source == nil {
			t.Fatal("missing recorded result", err)
		}
		snapshot, err := parallelworkspace.ReadResult(reader, request, *source)
		if err != nil {
			t.Fatal(err)
		}
		if got := gitOutput(t, checkout.Path, "show", snapshot.Record.SnapshotSHA+":main.txt"); got != "working" {
			t.Fatal("dirty result lost", got)
		}
		if got := gitOutput(t, checkout.Path, "show", snapshot.Record.SnapshotSHA+":committed.txt"); got != "branch commit" {
			t.Fatal("committed result lost", got)
		}
		if got := gitOutput(t, checkout.Path, "rev-list", "--parents", "-n", "1", snapshot.Record.SnapshotSHA); got != snapshot.Record.SnapshotSHA+" "+request.Seed.SnapshotSHA {
			t.Fatal("result carried intermediate branch history", got)
		}
		if got := gitOutput(t, checkout.Path, "ls-tree", snapshot.Record.SnapshotSHA, "--", "private.key"); got != "" {
			t.Fatal("result included excluded committed path", got)
		}
		wrong := request
		wrong.Status = journal.BranchCancelled
		if _, err := parallelworkspace.ReadResult(reader, wrong, *source); err == nil {
			t.Fatal("substituted result status admitted")
		}
		childWorkspaceWrite(t, checkout.Path, "main.txt", []byte("later live edits\n"))
	}
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, event := range events {
		if event.Runner["kind"] == ParentForkResultKind {
			seen[event.Branch] = true
		}
		if event.Type == journal.EventBranchFinished && !seen[event.Branch] {
			t.Fatal("branch finished before durable result")
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := journal.Recover(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := r.verifyParallelForkResults(t.Context(), reopened, frame.in, parallel, runtime, outcomes); err != nil {
		t.Fatal("result replay", err)
	}
	if captures != 2 {
		t.Fatal("restart recaptured mutable workspaces", captures)
	}
	for _, checkout := range checkouts {
		childWorkspaceRead(t, checkout.Path, "main.txt", []byte("later live edits\n"))
	}
	wrong := *outcomes[0]
	wrong.status = journal.BranchNoOutput
	if err := r.captureParallelForkResult(t.Context(), reopened, frame.in, parallel, wrong, true); err == nil {
		t.Fatal("restart changed result status")
	}
}
