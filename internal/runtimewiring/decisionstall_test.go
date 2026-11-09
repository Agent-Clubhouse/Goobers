package runtimewiring

import (
	"log/slog"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
)

func TestDecisionStallObserverIsOffUnlessOptedIn(t *testing.T) {
	settings := func(mode decisiongate.Mode, stall bool) *instance.Config {
		return &instance.Config{DecisionGate: &decisiongate.Settings{
			Mode: mode, StallDetection: stall, BaseURLEnv: "STALL_TEST_URL", KeyEnv: "STALL_TEST_KEY",
			ModelEnv: "STALL_TEST_MODEL", Fallback: decisiongate.FallbackAgent,
		}}
	}
	t.Setenv("STALL_TEST_URL", "http://127.0.0.1:1")
	t.Setenv("STALL_TEST_KEY", "k")
	t.Setenv("STALL_TEST_MODEL", "m")
	for name, cfg := range map[string]*instance.Config{
		"nil config":     nil,
		"no gate":        {},
		"off":            settings(decisiongate.ModeOff, true),
		"enforce":        settings(decisiongate.ModeEnforce, true),
		"shadow no flag": settings(decisiongate.ModeShadow, false),
	} {
		if DecisionStallObserver(cfg, slog.Default()) != nil {
			t.Fatalf("%s: observer must be nil", name)
		}
	}
	obs := DecisionStallObserver(settings(decisiongate.ModeShadow, true), slog.Default())
	if obs == nil {
		t.Fatal("opted-in shadow stall detection returned no observer")
	}
	env := apiv1.InvocationEnvelope{TaskID: "run:stage"}
	obs.ObserveWindow(env, "process-output/1", []byte("loop\nloop\nloop\nloop\nloop\nloop\n"))
	obs.FinishStage(env, true)
}
