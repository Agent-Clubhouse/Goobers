package frame

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/terminalhooks"
)

type recorder struct{ events []journal.Event }

func (r *recorder) AppendBestEffort(ev journal.Event) { r.events = append(r.events, ev) }

// rig records every hook call in order so the ORDER itself is asserted.
type rig struct {
	order       []string
	existingFix []terminalhooks.ExistingFixOutcome
	blocked     []terminalhooks.BlockedOutcome
	failed      []terminalhooks.FailedOutcome
	escalated   []string
	failEvery   error
	log         recorder
}

func (r *rig) hooks() *Hooks {
	step := func(name string) func(context.Context, Outcome) error {
		return func(context.Context, Outcome) error { r.order = append(r.order, name); return r.failEvery }
	}
	return &Hooks{
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		ExistingFix: func(_ context.Context, o terminalhooks.ExistingFixOutcome) error {
			r.order = append(r.order, "existingFix")
			r.existingFix = append(r.existingFix, o)
			return r.failEvery
		},
		Blocked: func(_ context.Context, o terminalhooks.BlockedOutcome) error {
			r.order = append(r.order, "blocked")
			r.blocked = append(r.blocked, o)
			return r.failEvery
		},
		Failed: func(_ context.Context, o terminalhooks.FailedOutcome) error {
			r.order = append(r.order, "failed")
			r.failed = append(r.failed, o)
			return r.failEvery
		},
		Escalate: func(_ context.Context, _ Outcome, item, stage, _ string) error {
			r.order = append(r.order, "escalate")
			r.escalated = append(r.escalated, item+"@"+stage)
			return r.failEvery
		},
		ClaimedItems: func(string) ([]string, error) { return []string{"claimed-7"}, nil },
		Prepare:      step("prepare"), Notify: step("notify"), Finalize: step("finalize"), Attribute: step("attribute"),
		Record: &r.log,
	}
}

func blockedOutcome() Outcome {
	return Outcome{
		RunID: "run-1", Gaggle: "web", Workflow: "implementation", Phase: journal.PhaseEscalated,
		Item: &apiv1.BacklogItem{ID: "42"},
		Result: engine.RunResult{
			Status: engine.StatusEscalated, FinalState: "implement",
			Outputs: map[string]apiv1.ResultEnvelope{"implement": {Status: apiv1.ResultBlocked, Summary: "waiting on 41"}},
		},
	}
}

// TestHooksFireInTheRunnersOrder: finalize releases the claims every hook above
// it resolves the driving item from, so it must come last among the required
// publication hooks.
func TestHooksFireInTheRunnersOrder(t *testing.T) {
	r := &rig{}
	r.hooks().Run(t.Context(), blockedOutcome())
	want := []string{"escalate", "blocked", "prepare", "notify", "finalize"}
	if !slices.Equal(r.order, want) {
		t.Fatalf("order = %v, want %v", r.order, want)
	}
	if len(r.blocked) != 1 || r.blocked[0].ItemID != "42" || r.blocked[0].Reason != "waiting on 41" {
		t.Fatalf("blocked outcome = %+v", r.blocked)
	}
}

func TestEveryHookFailureIsRecordedAndNeverSkipsTheRest(t *testing.T) {
	r := &rig{failEvery: errors.New("boom")}
	out := blockedOutcome()
	out.Backprop = true
	r.hooks().Run(t.Context(), out)
	want := []string{"escalate", "blocked", "prepare", "notify", "finalize", "attribute"}
	if !slices.Equal(r.order, want) {
		t.Fatalf("a failing hook skipped a later one: %v, want %v", r.order, want)
	}
	if len(r.log.events) != len(want) {
		t.Fatalf("recorded %d failures, want %d", len(r.log.events), len(want))
	}
	for _, ev := range r.log.events {
		if ev.RunID != "run-1" || ev.Gaggle != "web" || ev.Workflow != "implementation" || ev.Runner["driver"] != string(journal.DriverEngine) {
			t.Fatalf("failure not stamped with the run identity: %+v", ev)
		}
	}
}

func TestBackpropIsGatedAndHooksAreOptional(t *testing.T) {
	r := &rig{}
	r.hooks().Run(t.Context(), Outcome{RunID: "run-1", Phase: journal.PhaseCompleted, Result: engine.RunResult{Status: engine.StatusCompleted}})
	if slices.Contains(r.order, "attribute") {
		t.Fatalf("attribution ran for a non-backprop run: %v", r.order)
	}
	var nilHooks *Hooks
	nilHooks.Run(t.Context(), Outcome{})
	(&Hooks{}).Run(t.Context(), blockedOutcome())
}

func TestExistingFixOnlyForACompletedImplementNoWorkWithACommit(t *testing.T) {
	completed := func(env apiv1.ResultEnvelope, phase journal.RunPhase) Outcome {
		return Outcome{RunID: "run-1", Phase: phase, Item: &apiv1.BacklogItem{ID: "9"}, Result: engine.RunResult{
			Status: engine.StatusCompleted, Outputs: map[string]apiv1.ResultEnvelope{"implement": env},
		}}
	}
	withCommit := apiv1.ResultEnvelope{Status: apiv1.ResultNoWork, Outputs: map[string]interface{}{"existingFixCommit": "abc123"}}
	for name, tc := range map[string]struct {
		out  Outcome
		want int
	}{
		"fires":             {completed(withCommit, journal.PhaseCompleted), 1},
		"no commit":         {completed(apiv1.ResultEnvelope{Status: apiv1.ResultNoWork}, journal.PhaseCompleted), 0},
		"not no-work":       {completed(apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: withCommit.Outputs}, journal.PhaseCompleted), 0},
		"not completed run": {completed(withCommit, journal.PhaseFailed), 0},
	} {
		r := &rig{}
		r.hooks().Run(t.Context(), tc.out)
		if len(r.existingFix) != tc.want {
			t.Errorf("%s: existing-fix calls = %d, want %d", name, len(r.existingFix), tc.want)
		}
	}
}

func TestSelfBlockersAreDroppedAndAnnotated(t *testing.T) {
	r := &rig{}
	out := blockedOutcome()
	out.Result.Outputs["implement"] = apiv1.ResultEnvelope{Status: apiv1.ResultBlocked, Outputs: map[string]interface{}{terminalhooks.OutputBlockedBy: "#42, 41"}}
	r.hooks().Run(t.Context(), out)
	if len(r.blocked) != 1 || !slices.Equal(r.blocked[0].Blockers, []string{"41"}) {
		t.Fatalf("blockers = %+v, want only 41", r.blocked)
	}
	var annotated bool
	for _, ev := range r.log.events {
		annotated = annotated || (ev.Type == journal.EventRunnerAnnotation && ev.Runner["kind"] == terminalhooks.SelfBlockerDroppedKind)
	}
	if !annotated {
		t.Fatal("the dropped self-reference was not annotated")
	}
}

func TestFailedOutcomeCarriesStageCodeAndInfraFaultClass(t *testing.T) {
	r := &rig{}
	r.hooks().Run(t.Context(), Outcome{
		RunID: "run-1", Phase: journal.PhaseFailed,
		Result: engine.RunResult{Status: engine.StatusFailed, FinalState: "ci", FailureCode: "CI_FAILED", FailureMessage: "tests failed"},
	})
	if len(r.failed) != 1 || r.failed[0].Stage != "ci" || r.failed[0].Code != "CI_FAILED" || r.failed[0].Cause != "tests failed" || r.failed[0].FaultClass != "" {
		t.Fatalf("failed outcome = %+v", r.failed)
	}

	// A walk-level error has no result: the error is the cause, and a typed
	// infrastructure failure keeps its class.
	walk := &rig{}
	walk.hooks().Run(t.Context(), Outcome{RunID: "run-2", Phase: journal.PhaseFailed, Err: errors.New("max steps")})
	if len(walk.failed) != 1 || walk.failed[0].Code != WalkFailureCode || walk.failed[0].Cause != "max steps" {
		t.Fatalf("walk failure = %+v", walk.failed)
	}
}

func TestSelfExecutionDeniedParksTheItemAndIsObserved(t *testing.T) {
	denied := temporal.NewNonRetryableApplicationError("self execution denied", runsemantics.SelfExecutionDeniedCode, errors.New("refused"))
	if !engine.IsSelfExecutionDenied(fmt.Errorf("wrapped: %w", denied)) || engine.IsSelfExecutionDenied(errors.New("other")) {
		t.Fatal("engine.IsSelfExecutionDenied misclassifies")
	}
	if FailureCode(denied) != runsemantics.SelfExecutionDeniedCode || FaultClass(errors.New("x")) != "" {
		t.Fatal("failure classification is wrong")
	}
	r := &rig{}
	var observed bool
	h := r.hooks()
	h.SelfExecutionObserved = func(v bool) { observed = v }
	h.Run(t.Context(), Outcome{RunID: "run-1", Phase: journal.PhaseFailed, Item: &apiv1.BacklogItem{ID: "5"}, Err: denied, Result: engine.RunResult{FinalState: "implement"}})
	if !observed || len(r.blocked) != 1 || r.blocked[0].ItemID != "5" {
		t.Fatalf("observed=%v blocked=%+v, want the denied run parked and observed", observed, r.blocked)
	}
	if want := []string{"escalate", "blocked", "failed", "prepare", "notify", "finalize"}; !slices.Equal(r.order, want) {
		t.Fatalf("order = %v, want %v", r.order, want)
	}
}

func TestPhaseForNeverSkipsTheFrame(t *testing.T) {
	for status, want := range map[string]journal.RunPhase{engine.StatusCompleted: journal.PhaseCompleted, engine.StatusEscalated: journal.PhaseEscalated, "": journal.PhaseFailed, "unknown-vocabulary": journal.PhaseFailed} {
		if got := PhaseFor(engine.RunResult{Status: status}); got != want {
			t.Errorf("PhaseFor(%q) = %q, want %q", status, got, want)
		}
	}
	_ = telemetry.ErrorClassInfra
}
