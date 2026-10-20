package v1alpha1

import "testing"

func TestChildWorkflowPolicyDefaultsAndCopy(t *testing.T) {
	task := Task{ChildWorkflows: &ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}, AllowedCapabilities: []string{"repo:read"}}}
	if got := task.ChildWorkflows.EffectiveMaxChildren(); got != 4 {
		t.Fatalf("default=%d", got)
	}
	if task.ChildWorkflows.MaxChildren != 0 || task.ChildWorkflows.AllowPRPublication {
		t.Fatal("defaults mutate authored policy or grant publication")
	}
	copied := task.DeepCopy()
	copied.ChildWorkflows.AllowedGoobers[0] = "reviewer"
	copied.ChildWorkflows.AllowedCapabilities[0] = "repo:push"
	copied.ChildWorkflows.MaxChildren = 32
	if task.ChildWorkflows.AllowedGoobers[0] != "coder" || task.ChildWorkflows.AllowedCapabilities[0] != "repo:read" || task.ChildWorkflows.MaxChildren != 0 {
		t.Fatal("deepcopy aliases child authority")
	}
	if got := copied.ChildWorkflows.EffectiveMaxChildren(); got != 32 {
		t.Fatalf("explicit maximum=%d", got)
	}
}

func TestChildSelectionPolicyDeepCopy(t *testing.T) {
	task := Task{ChildWorkflows: &ChildWorkflowPolicy{
		AllowedAgentProfiles: []ChildAgentProfile{{Name: "fast", Goober: "coder", Harness: HarnessCopilot, Model: "configured-model"}},
		AllowedBases:         []ChildBaseReference{{Name: "integration", Ref: "refs/heads/integration"}},
	}}
	copy := task.DeepCopy()
	copy.ChildWorkflows.AllowedAgentProfiles[0].Model = "another-model"
	copy.ChildWorkflows.AllowedBases[0].Ref = "refs/tags/release"
	if task.ChildWorkflows.AllowedAgentProfiles[0].Model != "configured-model" || task.ChildWorkflows.AllowedBases[0].Ref != "refs/heads/integration" {
		t.Fatal("deepcopy aliases selection authority")
	}
}
