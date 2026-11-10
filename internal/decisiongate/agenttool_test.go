package decisiongate

import "testing"

func TestAgentToolDeniedByDefaultAndValidated(t *testing.T) {
	var none *AgentTool
	if none.Granted("review") || (&AgentTool{}).Granted("review") {
		t.Fatal("absent or empty agentTool must deny")
	}
	a := &AgentTool{Tasks: []string{"review"}}
	if !a.Granted("review") || a.Granted("other") || a.Granted("") {
		t.Fatal("grant must match exact task ids only")
	}
	if a.EffectiveMaxCalls() != DefaultAgentToolMaxCalls {
		t.Fatal("default budget not applied")
	}
	for _, bad := range []*AgentTool{{Tasks: []string{"*"}}, {Tasks: []string{""}}, {MaxCalls: -1}, {MaxCalls: MaxAgentToolMaxCalls + 1}} {
		if bad.validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
