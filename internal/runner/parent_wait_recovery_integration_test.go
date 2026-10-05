//go:build integration

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type acceptedRecoveryFixture struct {
	*childHandoffFixture
	request ChildHandoffRequest
}

func (f *acceptedRecoveryFixture) Recover(context.Context, apiv1.InvocationEnvelope) (ChildHandoffRequest, error) {
	return f.request, nil
}

func TestIntegrationAcceptedRecoveryWaitsBeforeAnyContinuation(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "crash", true: "human-reopen"}[terminal], func(t *testing.T) {
			r, run, frame, f, fork := prepareChildWaitRuntime(t)
			r.cfg.ChildWorkflowRecoveryAdmission = func(*journal.Reader) error { return nil }
			if err := frame.recordTaskStartedWithRecovery(1, "", 0, 0, newStageUsageTotals()); err != nil {
				t.Fatal(err)
			}
			workspace, err := r.createStageWorkspace(t.Context(), frame.in, frame.t.Name, apiv1.WorkspaceRepo, false, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(workspace.path, "pending.txt"), []byte("preserve parent edits"), 0600); err != nil {
				t.Fatal(err)
			}
			custody, err := workspace.worktree.HoldForChild(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			env := apiv1.InvocationEnvelope{RunID: frame.in.RunID, Gaggle: frame.in.Gaggle, ChildWorkflowOrigin: frame.childOrigin, InstructionAddendum: "original guidance"}
			f.envs = []apiv1.InvocationEnvelope{env}
			request := ChildHandoffRequest{Gaggle: "web", ParentRunID: frame.in.RunID, Action: "wait", RequestID: journal.Digest([]byte("request")), ChildRunID: "child-run", AcceptanceID: "trigger-child-run", InvocationKey: "work", SourceDigest: journal.Digest([]byte("source")), Origin: *frame.childOrigin}
			r.cfg.ChildHandoff = &acceptedRecoveryFixture{f, request}
			if terminal {
				if err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
					t.Fatal(err)
				}
			}
			if err = RecordContainedParentRecovery(run, journal.Event{Stage: frame.t.Name, Attempt: 1, Seq: run.Seq()}, journal.Digest([]byte("contract")), ContainedParentWorkspaceCustody{Version: 1, Origin: frame.childOrigin, Workspace: custody}, env, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err = RecoverContainedParentWait(run, request); err != nil {
				t.Fatal(err)
			}
			if terminal {
				if err = run.Append(journal.Event{Type: journal.EventRunResumed, Status: string(journal.PhaseRunning)}); err != nil {
					t.Fatal(err)
				}
			}
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			start := int32(1)
			var accounting *resumeRetryAccounting
			if !terminal {
				resumed, ok := recoverChildTaskContext(events, frame.t.Name)
				if !ok || resumed.childWaitErr != nil {
					t.Fatal(resumed)
				}
				frame.childWaitResume, frame.childWaitAttempt, frame.childWaitClass = resumed.childWait, resumed.attempt, resumed.class
				start = int32(resumed.attempt) + 1
				accounting = &resumeRetryAccounting{policyAttempts: resumed.childWait.PolicyAttempts, infrastructureFailures: resumed.childWait.InfrastructureFailures, replacementConsumesPolicy: true}
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := r.runTask(t.Context(), frame, 0, start, "", "", nil, false, accounting)
				done <- err
			}()
			select {
			case <-f.waiting:
			case err := <-done:
				t.Fatal("did not wait", err)
			case <-time.After(10 * time.Second):
				t.Fatal("wait not restored")
			}
			f.mu.Lock()
			calls := len(f.envs)
			f.mu.Unlock()
			if calls != 1 {
				t.Fatal("continuation dispatched before child settled", calls)
			}
			close(f.complete)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("continuation did not finish")
			}
			f.mu.Lock()
			envs := append([]apiv1.InvocationEnvelope(nil), f.envs...)
			f.mu.Unlock()
			if len(envs) != 2 || envs[1].Attempt != 2 || envs[1].ChildWorkflowOrigin.StageOccurrence != env.ChildWorkflowOrigin.StageOccurrence || envs[1].InstructionAddendum != "original guidance" {
				t.Fatal(envs)
			}
			fork.assertParentUnchanged(t)
		})
	}
}
