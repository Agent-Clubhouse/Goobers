package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

func TestDecisionShadowObserverIsOffUnlessOptedIn(t *testing.T) {
	if newDecisionShadowObserver(nil, nil) != nil || newDecisionShadowObserver(&instance.Config{}, nil) != nil {
		t.Fatal("absent config must yield no observer")
	}
	enforce := &instance.Config{DecisionGate: &decisiongate.Settings{Mode: decisiongate.ModeEnforce}}
	if newDecisionShadowObserver(enforce, nil) != nil {
		t.Fatal("only shadow mode installs this observer")
	}
	bad := &instance.Config{DecisionGate: &decisiongate.Settings{Mode: decisiongate.ModeShadow, BaseURLEnv: "GOOBERS_TEST_UNSET_URL", KeyEnv: "GOOBERS_TEST_UNSET_KEY", ModelEnv: "GOOBERS_TEST_UNSET_MODEL", Fallback: decisiongate.FallbackAgent}}
	if newDecisionShadowObserver(bad, nil) != nil {
		t.Fatal("unresolvable endpoint must degrade to no observer, not fail the run")
	}
}

func TestDecisionShadowInputValidityUsesHandoffValidationOutput(t *testing.T) {
	result := apiv1.ResultEnvelope{Outputs: map[string]interface{}{
		handoffcheck.OutputKey: handoffcheck.Report{InputValid: handoffcheck.InputValidFalse},
	}}
	valid, known := decisionShadowInputValidity(result)
	if !known || valid {
		t.Fatalf("valid=%v known=%v", valid, known)
	}
	if got := shadowInputValidity(decisiongate.ShadowRecord{}); got != string(handoffcheck.InputValidUnknown) {
		t.Fatalf("unknown = %q", got)
	}
	if got := shadowInputValidity(decisiongate.ShadowRecord{InputKnown: true, InputValid: true}); got != string(handoffcheck.InputValidTrue) {
		t.Fatalf("true = %q", got)
	}
}

// The shipped claimed-item schema must accept what `backlog-query --claim`
// actually writes, or binding it would report every healthy handoff invalid.
func TestShippedClaimedItemHandoffSchemaMatchesBacklogQueryOutput(t *testing.T) {
	cfg := &instance.Config{DecisionGate: &decisiongate.Settings{Mode: decisiongate.ModeShadow}}
	schema, err := newHandoffSchemaLoader(cfg, "../../reference-workflows")("gaggles/goobers/schemas/claimed-item.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := marshalClaimedBacklogItems([]providers.WorkItem{{Provider: providers.ProviderGitHub, ID: "6733", Title: "bind handoff schemas", Labels: []string{"ready"}}}, nil, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if verdict := schema.Check(claimed); !verdict.Valid {
		t.Fatalf("real claimed item rejected: %+v", verdict.Issues)
	}
	if verdict := schema.Check([]byte(`{"provider":"github","title":"no id"}`)); verdict.Valid {
		t.Fatal("claimed item without an id accepted")
	}
}

func TestRunnerConfigWiresHandoffSchemasOnlyWhileGateIsOn(t *testing.T) {
	bindings := []decisiongate.HandoffSchemaBinding{{Workflow: "implementation", Stage: "query-backlog", SchemaPath: "s.json"}}
	for mode, want := range map[decisiongate.Mode]string{decisiongate.ModeOff: "", decisiongate.ModeShadow: "s.json"} {
		var rc runner.Config
		applyRunnerConfigFinalizers(&rc, runnerCompositionInput{Config: &instance.Config{DecisionGate: &decisiongate.Settings{Mode: mode, HandoffSchemas: bindings}}}, nil)
		if got := rc.ResultHandoffSchemas["implementation"]["query-backlog"]; got != want {
			t.Errorf("mode %s: bound schema = %q, want %q", mode, got, want)
		}
	}
}
