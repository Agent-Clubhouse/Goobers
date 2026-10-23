package engine

import (
	"testing"

	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
)

func TestReviewMissingMemoMustNotAdoptUnknownExecution(t *testing.T) {
	f := &fakeStarter{err: serviceerror.NewWorkflowExecutionAlreadyStarted("exists", "r", "e"), desc: &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{}}}
	s := &TemporalStarter{client: f, taskQueue: "cloud", admission: testStartAdmission{}}
	r, e := s.Start(t.Context(), cloudStartInput(9))
	if e != nil || r.Adopt || !r.NotMine {
		t.Fatalf("adopted execution with no acceptance/owner evidence: %+v", r)
	}
}
func TestReviewUnpersistedMatchingIDsMustNotStart(t *testing.T) {
	in := cloudStartInput(9)
	f := &fakeStarter{run: fakeRun{id: in.RunID, runID: "exec"}}
	s := &TemporalStarter{client: f, taskQueue: "cloud"}
	r, e := s.Start(t.Context(), in)
	if e == nil && f.gotOpts.ID != "" {
		t.Fatalf("cloud start used only caller-supplied matching strings; no acceptance row exists: %+v", r)
	}
}
func TestReviewLocalRemotePlacementIsNotCloudAdmission(t *testing.T) {
	in := sampleInput()
	in.SelfExecutionDenied = true
	f := &fakeStarter{run: fakeRun{id: in.RunID}}
	s := &TemporalStarter{client: f, taskQueue: "local"}
	_, e := s.Start(t.Context(), in)
	if e != nil {
		t.Fatalf("local daemon with remote-only placement rejected by cloud guard: %v", e)
	}
}

func TestStartFenceRefusesUnverifiableOwner(t *testing.T) {
	for _, mode := range []string{"acceptance", "newer", "epoch-zero", "starter", "instance", "input", "type", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			in := cloudStartInput(7)
			owner := StartOwnerMemo{AcceptanceID: in.AcceptanceID, OwnerEpoch: 7, StarterID: in.StarterID}
			if mode == "acceptance" {
				owner.AcceptanceID = "foreign"
			}
			if mode == "newer" {
				owner.OwnerEpoch = 8
			}
			if mode == "epoch-zero" {
				owner.OwnerEpoch = 0
			}
			if mode == "starter" {
				owner.StarterID = "foreign"
			}
			starter := existingStart(t, owner)
			fake := starter.client.(*fakeStarter)
			if mode == "instance" {
				in.InstanceID = "foreign"
			}
			if mode == "input" {
				in.WorkflowName = "foreign"
			}
			if mode == "type" {
				fake.desc.WorkflowExecutionInfo.Type.Name = "ForeignWorkflow"
			}
			if mode == "malformed" {
				fake.desc.WorkflowExecutionInfo.Memo.Fields[StartOwnerMemoKey].Data = []byte("invalid-json")
			}
			result, err := starter.Start(t.Context(), in)
			if err == nil && !result.NotMine {
				t.Fatalf("unverifiable execution accepted: %+v", result)
			}
		})
	}
}
