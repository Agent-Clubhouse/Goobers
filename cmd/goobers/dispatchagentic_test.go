package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
)

// The kit must carry the capability→env mapping the harness depends on.
//
// A harness reads its credential from the environment (agent:model becomes
// COPILOT_GITHUB_TOKEN). On the worker that variable is ambient, supplied by
// the deployment; a pod has none by design. MEASURED before the pod applied
// the mapping:
//
//	harness "copilot" preflight failed in pod:
//	  Error: No authentication information found.
//
// If EnvCapabilities ever stops carrying agent:model, an agentic pod fails at
// preflight with an error about GitHub auth that says nothing about kits.
func TestKitCarriesTheHarnessCredentialMapping(t *testing.T) {
	envCaps := buildEnvCapabilities()
	got, ok := envCaps["agent:model"]
	if !ok {
		t.Fatal("agent:model has no env mapping; an agentic pod cannot authenticate its harness")
	}
	if got != copilotModelEnv {
		t.Fatalf("agent:model maps to %q, want %q", got, copilotModelEnv)
	}
}

// The mapping has to survive the kit round trip, since that is how it reaches
// the pod at all.
func TestEnvCapabilitiesSurviveTheKit(t *testing.T) {
	kit := &agentickit.Kit{
		Envelope:        apiv1.InvocationEnvelope{RunID: "r", Goober: "g"},
		EnvCapabilities: buildEnvCapabilities(),
	}
	data, digest, err := agentickit.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	back, err := agentickit.Unmarshal(data, digest)
	if err != nil {
		t.Fatal(err)
	}
	if back.EnvCapabilities["agent:model"] != copilotModelEnv {
		t.Fatalf("mapping lost in transport: %v", back.EnvCapabilities)
	}
}
