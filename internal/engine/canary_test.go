package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// canary_test.go proves the #2931 fail-closed dispatch canary
// (distributed-state-and-coordination.md §11 / §13 item 7): no known
// credential value may appear in a serialized dispatch envelope, and an
// envelope carrying one refuses the stage rather than executing. The canary's
// registry is the SAME exact-value registry every resolver-issued and
// credential-plane-minted value is registered with (the wiring hands both the
// one shared journal.RegistryScrubber), so registering a value here is
// exactly what a plane mint does to the production canary.

type recordingAutomated struct{ calls int }

func (r *recordingAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	r.calls++
	return "pass", nil
}

func poisonedEnvelope(secret string) apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{
		RunID:  "run-canary",
		TaskID: "implement",
		Inputs: map[string]any{"leak": "prefix " + secret + " suffix"},
	}
}

func canaryRegistry(t *testing.T, secret string) *journal.RegistryScrubber {
	t.Helper()
	registry := journal.NewRegistryScrubber()
	// The same call the credential plane makes for every value it mints.
	registry.Register([]byte(secret))
	return registry
}

func assertCanaryRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("a dispatch envelope carrying a registered credential value executed; the canary must refuse it")
	}
	var applicationErr *temporal.ApplicationError
	if !errors.As(err, &applicationErr) {
		t.Fatalf("canary refusal is %T (%v), want a typed temporal.ApplicationError", err, err)
	}
	if !applicationErr.NonRetryable() {
		t.Errorf("canary refusal is retryable; a retry re-dispatches the identical leaked envelope")
	}
	if applicationErr.Type() != FailureTypeStage {
		t.Errorf("canary refusal type = %q, want %q", applicationErr.Type(), FailureTypeStage)
	}
	if !strings.Contains(err.Error(), "#2931") || !strings.Contains(err.Error(), "implement") {
		t.Errorf("canary refusal %q must cite #2931 and name the stage", err.Error())
	}
	if strings.Contains(err.Error(), "sekret") {
		t.Errorf("canary refusal %q echoes the leaked value", err.Error())
	}
}

func TestDispatchCanaryRefusesLeakedEnvelopes(t *testing.T) {
	const secret = "sekret-canary-value-1234567890"
	auto := &recordingAutomated{}
	invoked := 0
	activities := &Activities{
		Goober: &fakeInvoker{
			invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				invoked++
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			},
			review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
				invoked++
				return apiv1.Verdict{}, nil
			},
		},
		Det: &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			invoked++
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		}},
		Auto:   auto,
		Canary: canaryRegistry(t, secret),
	}
	env := poisonedEnvelope(secret)
	ctx := context.Background()

	_, err := activities.InvokeGoober(ctx, env, "goobers/wf/run-canary", "", "", "")
	assertCanaryRefusal(t, err)
	_, err = activities.ReviewGoober(ctx, env, "goobers/wf/run-canary", "", "", "", false)
	assertCanaryRefusal(t, err)
	_, err = activities.RunDeterministic(ctx, env, apiv1.DeterministicRun{Command: []string{"true"}}, "goobers/wf/run-canary", "")
	assertCanaryRefusal(t, err)
	_, err = activities.EvaluateAutomated(ctx, apiv1.AutomatedGate{}, env)
	assertCanaryRefusal(t, err)

	if invoked != 0 || auto.calls != 0 {
		t.Fatalf("a refused envelope reached an execution seam (invoked=%d auto=%d)", invoked, auto.calls)
	}
}

// TestDispatchCanaryPassesCleanEnvelopes proves the canary is a tripwire, not
// a tax: an envelope with no registered value executes normally, including
// when the registry is non-empty.
func TestDispatchCanaryPassesCleanEnvelopes(t *testing.T) {
	auto := &recordingAutomated{}
	activities := &Activities{
		Auto:   auto,
		Canary: canaryRegistry(t, "sekret-canary-value-1234567890"),
	}
	outcome, err := activities.EvaluateAutomated(context.Background(), apiv1.AutomatedGate{}, apiv1.InvocationEnvelope{
		RunID:  "run-clean",
		TaskID: "gate",
		Inputs: map[string]any{"status": "success"},
	})
	if err != nil || outcome != "pass" || auto.calls != 1 {
		t.Fatalf("clean envelope: outcome=%q err=%v calls=%d", outcome, err, auto.calls)
	}
}

// TestDispatchCanaryNilIsDisabled pins the local-runner posture: with no
// canary wired (the in-process path that never serializes an envelope
// off-process), nothing is asserted and nothing breaks.
func TestDispatchCanaryNilIsDisabled(t *testing.T) {
	auto := &recordingAutomated{}
	activities := &Activities{Auto: auto}
	if _, err := activities.EvaluateAutomated(context.Background(), apiv1.AutomatedGate{},
		poisonedEnvelope("sekret-canary-value-1234567890")); err != nil {
		t.Fatalf("nil canary must not refuse: %v", err)
	}
	if auto.calls != 1 {
		t.Fatalf("auto.calls = %d, want 1", auto.calls)
	}
}
