package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func TestParentDispatchRetainsExactBranchWithoutExposingRecoveryInput(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-branch", "wrong-role", "wrong-attempt", "wrong-source", "host-path", "ended", "untrusted-ref", "missing-recovery"} {
		t.Run(mode, func(t *testing.T) {
			f := containedParentFixture(t)
			run, env := configuredChildStage(t, f)
			seq, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1, Branch: 2}, false)
			if err != nil {
				t.Fatal(err)
			}
			env.ChildWorkflowOrigin = origin
			env.Workspace = ""
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			id, err := reader.Identity()
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			var started journal.Event
			for _, event := range events {
				if event.Seq == seq {
					started = event
				}
			}
			recorder, err := runner.OwnedBranchRecorder(run, 2)
			if err != nil {
				t.Fatal(err)
			}
			store := childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}
			kit := []byte("retained kit fixture")
			kitDigest := journal.Digest(kit)
			if err := store.Put(t.Context(), kitDigest, kit); err != nil {
				t.Fatal(err)
			}
			contract := childpod.Contract{Version: 1, Identity: id, ParentOrigin: origin, ParentBranch: 2, Stage: "plan", Attempt: 1, PodAttempt: int(seq), StartedAt: started.Time, KitDigest: kitDigest, Ceiling: credentials.NewChildCeiling(false, env.Capabilities, env.Capabilities)}
			raw, err := json.Marshal(contract)
			if err != nil {
				t.Fatal(err)
			}
			digest := journal.Digest(raw)
			if err := store.Put(t.Context(), digest, raw); err != nil {
				t.Fatal(err)
			}
			retained := childpod.RetainedAttempt{Version: 1, Input: engine.ChildDispatchInput{Attempt: dispatcher.Attempt{InstanceID: id.InstanceID, RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow, Stage: "plan", Number: 1, PodAttempt: int(seq), Agentic: true, WorkflowParent: true, ChildExecutionDigest: digest, KitDigest: kitDigest, Envelope: &env}, Queue: "pinned-worker", Eligible: []dispatcher.RunnerSpec{{OS: "linux", HostKind: instance.RunnerHostImage}}}}
			blobs := &parentInvocationBlobs{ParentBlobs: store, recorder: recorder}
			before := run.Seq()
			switch mode {
			case "wrong-branch":
				blobs.recorder = run
			case "wrong-role":
				retained.Input.Attempt.WorkflowParent = false
			case "wrong-attempt":
				retained.Input.Attempt.PodAttempt++
			case "host-path":
				retained.Input.Attempt.Envelope.Workspace = t.TempDir()
			case "wrong-source":
				retained.Input.Attempt.Envelope.ConfigGeneration = journal.Digest([]byte("different generation"))
			case "ended":
				if err := recorder.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1}); err != nil {
					t.Fatal(err)
				}
				before = run.Seq()
			}
			err = blobs.keepAttempt(t.Context(), retained)
			if mode == "wrong-branch" || mode == "wrong-role" || mode == "wrong-attempt" || mode == "wrong-source" || mode == "host-path" || mode == "ended" {
				if err == nil || run.Seq() != before {
					t.Fatal("invalid dispatch changed custody", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "untrusted-ref" {
				blobs.retainedRef.Integrity = "untrusted"
			}
			if mode == "missing-recovery" {
				blobs.retainedRef = journal.Ref{}
			}
			before = run.Seq()
			err = blobs.BindContract(t.Context(), digest)
			if mode != "valid" {
				if err == nil || run.Seq() != before {
					t.Fatal("invalid recovery custody granted authority", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			pending, _, err := childpod.PendingParentScopes(t.Context(), reader)
			if err != nil || len(pending) != 1 || pending[digest].Event.Branch != 2 {
				t.Fatal("physical branch custody lost", pending, err)
			}
			restored, err := readParentRetainedAttempt(t.Context(), reader, store, blobs.retainedRef, contract, digest)
			if err != nil || restored.Input.BindingDigest() != retained.Input.BindingDigest() {
				t.Fatal("recovery input changed", err)
			}
			scope := childpod.ParentAttemptBlobs{Store: store, ContractDigest: digest}
			if _, err := scope.Get(t.Context(), kitDigest); err != nil {
				t.Fatal("worker lost its kit", err)
			}
			if _, err := scope.Get(t.Context(), blobs.retainedRef.Digest); !errors.Is(err, blobstore.ErrNotFound) {
				t.Fatal("worker can read host recovery custody", err)
			}
			before = run.Seq()
			if err := blobs.BindContract(t.Context(), digest); err == nil || run.Seq() != before {
				t.Fatal("duplicate physical writer admitted", err)
			}
		})
	}
}
