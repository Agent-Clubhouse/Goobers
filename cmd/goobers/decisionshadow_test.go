package main

import (
	"testing"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
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
