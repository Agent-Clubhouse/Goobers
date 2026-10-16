//go:build integration

package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

type stopHostJoinAfterReady struct{ parallelworkspace.Recorder }

var errStopHostJoin = errors.New("test: stop host join after application plan")

func (r stopHostJoinAfterReady) Append(event journal.Event) error {
	if err := r.Recorder.Append(event); err != nil {
		return err
	}
	if event.Runner["kind"] == spec.JoinKind && event.Runner["phase"] == "ready" {
		return errStopHostJoin
	}
	return nil
}

func interruptHostForkJoin(t *testing.T, restorer parentArchiveRestorer, run *journal.Run, reader *journal.Reader, plan runner.ParentForkPlan, reference journal.Ref, root *worktree.Worktree) recovery.Record {
	t.Helper()
	pending, err := runner.PendingParentForks(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := restorer.recoverForkPlans(t.Context(), run, reader, pending); err != nil {
		t.Fatal(err)
	}
	captureForkResultsForCleanup(t, restorer, run, reader, runner.ParentForkRetirement{Plan: plan, Reference: reference})
	results, err := runner.ParentForkResults(reader, plan, reference)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := parallelworkspace.ReadSource(reader, plan.Source, plan.Parallel, plan.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	project, err := recoveryConfiguredProject(restorer.config, seed.Record.RepositoryKey)
	if err != nil {
		t.Fatal(err)
	}
	request := spec.JoinRequest{Request: spec.Request{RunID: plan.RunID, Gaggle: plan.Gaggle, Parallel: plan.Parallel, Sequence: plan.Sequence, At: seed.Record.CreatedAt, Repository: project}, Plan: reference, Seed: plan.Source, Root: *plan.Root, Results: make([]spec.JoinResult, len(results))}
	for _, result := range results {
		request.Results[result.Branch-1] = spec.JoinResult{Branch: result.Branch, Status: result.Status, Custody: plan.Workspaces[result.Branch-1], Source: result.Source}
		if err := run.Append(journal.Event{Type: journal.EventBranchFinished, Parallel: plan.Parallel, Branch: result.Branch, BranchStatus: result.Status}); err != nil {
			t.Fatal(err)
		}
	}
	service := parallelworkspace.Service{Worktrees: restorer.worktrees, CloneURL: restorer.cloneURL}
	if err := service.Join(t.Context(), stopHostJoinAfterReady{run}, request); !errors.Is(err, errStopHostJoin) {
		t.Fatal("host join did not stop at durable application boundary", err)
	}
	// Simulate a crash after one intended file write, before atomic index update.
	writeFileContent(t, filepath.Join(root.Path, "source.txt"), "ordinary working\n")
	target, err := root.HeldCleanupTarget(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.ParentCleanupWorkspace(reader, target); !errors.Is(err, runner.ErrParentReturnPending) {
		t.Fatal("partial join was eligible for cleanup", err)
	}
	return preparedHostJoinRecord(t, reader)
}

func preparedHostJoinRecord(t *testing.T, reader *journal.Reader) recovery.Record {
	t.Helper()
	pending, err := parallelworkspace.PendingPreparations(reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range pending {
		if value.Value.Request.Join {
			return value.Value.Snapshot.Record
		}
	}
	t.Fatal("join did not record temporary snapshot ownership")
	return recovery.Record{}
}

func TestIntegrationHostForkJoinTerminalRecovery(t *testing.T) {
	testdep.Require(t, "git")
	verifyHostForkArchiveRecovery(t, "join")
}
