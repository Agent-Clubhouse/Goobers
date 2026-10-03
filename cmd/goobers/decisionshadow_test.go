package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/handoffcheck"
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

func TestDecisionShadowInputValidityUsesHandoffValidationOutput(t *testing.T) {
	result := apiv1.ResultEnvelope{Outputs: map[string]interface{}{
		handoffcheck.OutputKey: handoffcheck.Report{InputValid: handoffcheck.InputValidFalse},
	}}
	valid, known := decisionShadowInputValidity(result)
	if !known || valid {
		t.Fatalf("valid=%v known=%v", valid, known)
	}

	t.Run("CredentialsUseTheGrantPath", func(t *testing.T) {
		t.Setenv("TEST_DECISION_URL", "https://decision.example")
		t.Setenv("TEST_DECISION_KEY", "decision-secret-value")
		t.Setenv("TEST_DECISION_MODEL", "decision-model")
		settings := &decisiongate.Settings{
			Mode:       decisiongate.ModeShadow,
			BaseURLEnv: "TEST_DECISION_URL",
			KeyEnv:     "TEST_DECISION_KEY",
			ModelEnv:   "TEST_DECISION_MODEL",
			Fallback:   decisiongate.FallbackAgent,
		}
		cfg := &instance.Config{DecisionGate: settings}
		resolver, grants, err := buildRoleCredentials(cfg, nil, "", "", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		selected := buildGooberCredentialGrants("implementer", string(apiv1.HarnessCopilot), settings.CredentialKeys(), grants)
		if len(selected) != 3 {
			t.Fatalf("decision grants = %+v, want three goober-scoped grants", selected)
		}
		for _, grant := range selected {
			value, err := resolver.Resolve(context.Background(), grant.Ref)
			if err != nil {
				t.Fatalf("resolve %s: %v", grant.Capability, err)
			}
			if value == "" {
				t.Fatalf("resolve %s returned an empty provider value", grant.Capability)
			}
		}
	})

	t.Run("PodObserverUsesMintedValuesWithoutLoggingThem", func(t *testing.T) {
		const secret = "decision-secret-value"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
				t.Errorf("authorization = %q", got)
			}
			_, _ = io.WriteString(w, `{"model":"decision-model","answers":{"claims_bad_input":{"type":"noul","noul":0.9,"confidence":0.8}}}`)
		}))
		t.Cleanup(server.Close)
		settings := &decisiongate.Settings{
			Mode:       decisiongate.ModeShadow,
			BaseURLEnv: "TEST_DECISION_URL",
			KeyEnv:     "TEST_DECISION_KEY",
			ModelEnv:   "TEST_DECISION_MODEL",
			Fallback:   decisiongate.FallbackAgent,
		}
		minted := []dispatcher.MintedCredential{
			{Capability: decisiongate.CredentialBaseURL, Value: server.URL},
			{Capability: decisiongate.CredentialAPIKey, Value: secret},
			{Capability: decisiongate.CredentialModel, Value: "decision-model"},
		}
		var logs strings.Builder
		observer, wait := newPodDecisionShadowObserver(
			&agentickit.Kit{DecisionGate: settings},
			minted,
			slog.New(slog.NewTextHandler(&logs, nil)),
		)
		if observer == nil {
			t.Fatal("shadow-configured pod did not construct an observer")
		}
		observer(apiv1.InvocationEnvelope{RunID: "pod-shadow-run"}, apiv1.ResultEnvelope{
			Status: apiv1.ResultSuccess, Summary: "completed normally",
		})
		wait()
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("provider key appeared in observer logs: %s", logs.String())
		}
		for _, field := range []string{"outcome=success", "inputValid=unknown", "verdict=spurious", "probability=0.9", "confidence=0", "cached=false", "agentClaimedBad=false"} {
			if !strings.Contains(logs.String(), field) {
				t.Errorf("pod shadow record lacks %q: %s", field, logs.String())
			}
		}

		off, _ := newPodDecisionShadowObserver(&agentickit.Kit{}, minted, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if off != nil {
			t.Fatal("pod observer must remain off without an opt-in policy")
		}
	})
	if got := shadowInputValidity(decisiongate.ShadowRecord{}); got != string(handoffcheck.InputValidUnknown) {
		t.Fatalf("unknown = %q", got)
	}
	if got := shadowInputValidity(decisiongate.ShadowRecord{InputKnown: true, InputValid: true}); got != string(handoffcheck.InputValidTrue) {
		t.Fatalf("true = %q", got)
	}
}
