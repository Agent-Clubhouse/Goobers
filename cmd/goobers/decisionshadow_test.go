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

func TestCreditAdvisoryClassifierPinsConfiguredShadowModel(t *testing.T) {
	const (
		urlEnv   = "GOOBERS_TEST_ADVISORY_URL"
		keyEnv   = "GOOBERS_TEST_ADVISORY_KEY"
		modelEnv = "GOOBERS_TEST_ADVISORY_MODEL"
	)
	t.Setenv(urlEnv, "http://127.0.0.1:1")
	t.Setenv(keyEnv, "test-key")
	t.Setenv(modelEnv, "failure-classifier-v1")
	settings := &decisiongate.Settings{
		Mode: decisiongate.ModeShadow, BaseURLEnv: urlEnv, KeyEnv: keyEnv,
		ModelEnv: modelEnv, Fallback: decisiongate.FallbackAgent,
	}
	classifier := newCreditAdvisoryClassifier(&instance.Config{DecisionGate: settings}, nil)
	if classifier == nil || classifier.Gate == nil || classifier.Model != "failure-classifier-v1" {
		t.Fatalf("classifier = %+v, want pinned shadow classifier", classifier)
	}

	settings.Mode = decisiongate.ModeEnforce
	if classifier := newCreditAdvisoryClassifier(&instance.Config{DecisionGate: settings}, nil); classifier != nil {
		t.Fatalf("enforce classifier = %+v, want shadow-only integration", classifier)
	}
}
