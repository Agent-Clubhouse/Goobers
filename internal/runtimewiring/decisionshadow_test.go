package runtimewiring

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

const shadowTestKey = "shadow-test-key-4f9a2c71"

func podShadowDelivery(mode decisiongate.Mode, url string) *dispatcher.DecisionShadow {
	return &dispatcher.DecisionShadow{
		Settings: decisiongate.Settings{
			Mode: mode, BaseURLEnv: "SHADOW_URL", KeyEnv: "SHADOW_KEY", ModelEnv: "SHADOW_MODEL",
			Fallback: decisiongate.FallbackAgent,
		},
		BaseURL: url, APIKey: shadowTestKey, Model: "shadow-model",
	}
}

func TestPodDecisionShadowObserverIsOffUnlessShadow(t *testing.T) {
	if PodDecisionShadowObserver(nil, nil, nil) != nil {
		t.Fatal("no delivery must yield no observer")
	}
	for _, mode := range []decisiongate.Mode{"", decisiongate.ModeOff, decisiongate.ModeEnforce} {
		if PodDecisionShadowObserver(podShadowDelivery(mode, "https://example.invalid"), nil, nil) != nil {
			t.Fatalf("mode %q installed an observer", mode)
		}
	}
	unresolvable := podShadowDelivery(decisiongate.ModeShadow, "")
	if PodDecisionShadowObserver(unresolvable, nil, &bytes.Buffer{}) != nil {
		t.Fatal("an unresolvable endpoint must degrade to no observer")
	}
}

func TestPodDecisionShadowObserverScoresStagesWithLocalFields(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{decisiongate.ClaimQuestion: map[string]any{"type": "noul", "noul": 0.95}},
		})
	}))
	t.Cleanup(server.Close)

	registry := journal.NewRegistryScrubber()
	var stderr bytes.Buffer
	observer := PodDecisionShadowObserver(podShadowDelivery(decisiongate.ModeShadow, server.URL), registry, &stderr)
	if observer == nil {
		t.Fatal("shadow delivery did not install an observer")
	}
	env := apiv1.InvocationEnvelope{RunID: "run-1"}
	observer(env, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "the JSON is corrupted"})
	if calls.Load() != 1 {
		t.Fatalf("scoring was not finished before the observer returned: %d calls", calls.Load())
	}
	logged := stderr.String()
	for _, field := range []string{"decisiongate.shadow", "outcome=success", "inputValid=", "verdict=", "probability=", "confidence=", "cached=", "agentClaimedBad=true", "error="} {
		if !strings.Contains(logged, field) {
			t.Fatalf("shadow record missing %q: %s", field, logged)
		}
	}
	if strings.Contains(logged, shadowTestKey) {
		t.Fatalf("key reached the log: %s", logged)
	}
	if got := registry.Scrub([]byte(shadowTestKey)); bytes.Contains(got, []byte(shadowTestKey)) {
		t.Fatal("delivered key was not registered for scrubbing")
	}
}

func TestPodDecisionShadowObserverScrubsReflectedProviderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad credential "+shadowTestKey, http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	var stderr bytes.Buffer
	observer := PodDecisionShadowObserver(podShadowDelivery(decisiongate.ModeShadow, server.URL), journal.NewRegistryScrubber(), &stderr)
	if observer == nil {
		t.Fatal("no observer")
	}
	observer(apiv1.InvocationEnvelope{RunID: "run-2"}, apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Summary: "the input was corrupted"})
	if !strings.Contains(stderr.String(), "outcome=non-success") {
		t.Fatalf("failed attempt was not recorded: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), shadowTestKey) {
		t.Fatalf("reflected key reached stderr: %s", stderr.String())
	}
}

func TestPodAgenticExecutorInputCarriesTheObserver(t *testing.T) {
	called := false
	input := PodAgenticExecutorInput(PodExecutorWiring{
		Kit:      &agentickit.Kit{},
		Observer: func(apiv1.InvocationEnvelope, apiv1.ResultEnvelope) { called = true },
	})
	if input.Observer == nil {
		t.Fatal("observer was not carried into the executor input")
	}
	input.Observer(apiv1.InvocationEnvelope{}, apiv1.ResultEnvelope{})
	if !called {
		t.Fatal("observer is not the delivered one")
	}
}
