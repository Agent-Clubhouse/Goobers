//go:build integration

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParallelParentRecoveryFindsAcceptedChildBeforeReplacement(t *testing.T) {
	testdep.Require(t, "git")
	r, run, frame, handoff, fork := prepareChildWaitRuntime(t)
	par := seedChildRecoveryParallel(t, run)
	frame.jr = &branchJournal{run: run, branch: 1}
	if err := frame.recordTaskStartedWithRecovery(1, "", 0, 0, newStageUsageTotals()); err != nil {
		t.Fatal(err)
	}
	workspace, err := r.createStageWorkspace(t.Context(), frame.in, frame.t.Name, apiv1.WorkspaceRepo, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.path, "pending.txt"), []byte("preserve parent edits"), 0600); err != nil {
		t.Fatal(err)
	}
	custody, err := workspace.worktree.HoldForChild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{RunID: frame.in.RunID, Gaggle: frame.in.Gaggle, ChildWorkflowOrigin: frame.childOrigin, InstructionAddendum: "recovered branch guidance"}
	handoff.envs = []apiv1.InvocationEnvelope{env}
	request := ChildHandoffRequest{Gaggle: frame.in.Gaggle, ParentRunID: frame.in.RunID, Action: "wait", RequestID: journal.Digest([]byte("accepted")), ChildRunID: "child-run", AcceptanceID: "trigger-child-run", InvocationKey: "work", SourceDigest: journal.Digest([]byte("source")), Origin: *frame.childOrigin}
	r.cfg.ChildHandoff = &acceptedRecoveryFixture{handoff, request}
	if err := RecordContainedParentRecovery(run, journal.Event{Branch: 1, Stage: frame.t.Name, Attempt: 1, Seq: run.Seq()}, journal.Digest([]byte("contract")), ContainedParentWorkspaceCustody{Version: 1, Origin: frame.childOrigin, Workspace: custody}, env, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Deliberately no child wait marker: the host recovered the pod's output
	// immediately before the crash, but the accepted child was not rehydrated.
	dir := run.Dir()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	frame.in.parallelChild, err = r.parallelChildOwner(frame.in, par, events)
	if err != nil {
		t.Fatal(err)
	}
	frame.in.parallelSlot, _ = newParallelBranchSlots(1).tryAcquire()
	defer frame.in.parallelSlot.release()
	// Recovery is the same logical stage, even if its ordinary branch clock
	// has elapsed and there is no fresh step allowance left.
	par.spec.BranchTimeoutSeconds = 1
	par.branches[0].startedAt = time.Now().Add(-time.Hour)
	r.maxSteps = 0
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan parallelBranchResult, 1)
	registrar, _ := journal.DefaultScrubber()
	steps := &atomic.Int64{}
	go func() {
		done <- r.runParallelBranch(ctx, recovered, par, frame.in, par.branchSnapshot(0), nil, "", apiv1.ResultEnvelope{}, nil, "", registrar, newParallelBranchEventIndex(events, "fan").events(1), steps)
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	select {
	case <-handoff.waiting:
	case result := <-done:
		joined = true
		t.Fatal("accepted work was not recovered", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	handoff.mu.Lock()
	calls := len(handoff.envs)
	handoff.mu.Unlock()
	if calls != 1 {
		t.Fatal("replacement ran before accepted child settled", calls)
	}
	close(handoff.complete)
	result := <-done
	joined = true
	// The serial definition fixture intentionally retains its join refusal;
	// this verifies the real branch recovery path, not writable admission.
	if result.err == nil || !strings.Contains(result.err.Error(), `completed the run instead of routing to "@join"`) || steps.Load() != 0 {
		t.Fatal("recovery failed or consumed a new boundary", result.err, steps.Load())
	}
	handoff.mu.Lock()
	envs := append([]apiv1.InvocationEnvelope(nil), handoff.envs...)
	handoff.mu.Unlock()
	if len(envs) != 2 || envs[1].Attempt != 2 || envs[1].InstructionAddendum != env.InstructionAddendum || envs[1].ChildWorkflowOrigin.StageOccurrence != env.ChildWorkflowOrigin.StageOccurrence {
		t.Fatal("recovered continuation lost its original identity or guidance", envs)
	}
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Branch == 1 && event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultFailure) {
			t.Fatal("recovery charged an interrupted failure", event)
		}
	}
	fork.assertParentUnchanged(t)
}
