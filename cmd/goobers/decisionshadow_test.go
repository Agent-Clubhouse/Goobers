package main

import (
	"testing"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
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

func TestDaemonDeliversShadowSettingsOnlyToOptedInAgenticStages(t *testing.T) {
	t.Setenv("SHADOW_TEST_URL", "https://decider.invalid")
	t.Setenv("SHADOW_TEST_KEY", "shadow-delivery-key-77")
	t.Setenv("SHADOW_TEST_MODEL", "m")
	settings := &decisiongate.Settings{
		Mode: decisiongate.ModeShadow, BaseURLEnv: "SHADOW_TEST_URL", KeyEnv: "SHADOW_TEST_KEY",
		ModelEnv: "SHADOW_TEST_MODEL", Fallback: decisiongate.FallbackAgent,
	}
	registry := journal.NewRegistryScrubber()
	service := daemonCredentialService{config: &instance.Config{DecisionGate: settings}, shared: registry}
	agentic := stageResolution{profile: stageProfile{goober: "implementer"}}

	delivery := service.decisionShadowDelivery(agentic)
	if delivery == nil || delivery.APIKey != "shadow-delivery-key-77" || delivery.BaseURL != "https://decider.invalid" {
		t.Fatalf("agentic stage got %+v", delivery)
	}
	if got := registry.Scrub([]byte(delivery.APIKey)); string(got) == delivery.APIKey {
		t.Fatal("delivered key was not registered with the daemon scrubber")
	}
	if service.decisionShadowDelivery(stageResolution{profile: stageProfile{deterministic: true}}) != nil {
		t.Fatal("a deterministic stage received shadow settings")
	}
	for _, mode := range []decisiongate.Mode{"", decisiongate.ModeOff, decisiongate.ModeEnforce} {
		off := *settings
		off.Mode = mode
		service.config = &instance.Config{DecisionGate: &off}
		if service.decisionShadowDelivery(agentic) != nil {
			t.Fatalf("mode %q delivered settings", mode)
		}
	}
}

// The shipped claimed-item schema must accept what `backlog-query --claim`
// actually writes, or binding it would report every healthy handoff invalid.
func TestShippedClaimedItemHandoffSchemaMatchesBacklogQueryOutput(t *testing.T) {
	cfg := &instance.Config{DecisionGate: &decisiongate.Settings{Mode: decisiongate.ModeShadow}}
	schema, err := shadowRunnerConfig(t, cfg).HandoffSchemaLoader("gaggles/goobers/schemas/claimed-item.schema.json")
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
		rc := shadowRunnerConfig(t, &instance.Config{DecisionGate: &decisiongate.Settings{Mode: mode, HandoffSchemas: bindings}})
		if got := rc.ResultHandoffSchemas["implementation"]["query-backlog"]; got != want {
			t.Errorf("mode %s: bound schema = %q, want %q", mode, got, want)
		}
	}
}

// Exercise the production composition path after the shared loader moved into
// runtimewiring, without duplicating its schema resolution in the test.
func shadowRunnerConfig(t *testing.T, cfg *instance.Config) runner.Config {
	t.Helper()
	rc, _, err := buildRunnerConfig(runnerCompositionInput{
		Layout:         instance.NewLayout(t.TempDir()).WithConfigDir("../../reference-workflows"),
		Config:         cfg,
		SharedRegistry: journal.NewRegistryScrubber(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return rc
}
