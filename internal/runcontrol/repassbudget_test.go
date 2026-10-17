package runcontrol

import (
	"maps"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestRepassBudgetChargeBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		gate        apiv1.Gate
		outcome     string
		maxRepasses int
		bound       int
	}{
		{name: "default policy budget", outcome: "fail", bound: 3},
		{name: "inherited policy budget", outcome: "fail", maxRepasses: 2, bound: 2},
		{name: "gate overrides inherited policy budget", gate: apiv1.Gate{MaxRepasses: 1}, outcome: "fail", maxRepasses: 7, bound: 1},
		{name: "review verdict spends policy budget", outcome: string(apiv1.VerdictNeedsChanges), maxRepasses: 2, bound: 2},
		{name: "infrastructure ignores policy bounds", gate: apiv1.Gate{MaxRepasses: 1}, outcome: "infra", maxRepasses: 7, bound: 2},
		{name: "default polling budget", outcome: "timeout", maxRepasses: 1, bound: 12},
		{name: "unset polling override uses default", gate: apiv1.Gate{Automated: &apiv1.AutomatedGate{}}, outcome: "timeout", maxRepasses: 1, bound: 12},
		{name: "polling override ignores policy bounds", gate: apiv1.Gate{MaxRepasses: 1, Automated: &apiv1.AutomatedGate{MaxTimeoutPolls: 2}}, outcome: "timeout", maxRepasses: 7, bound: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A zero-value budget also represents a resumed history that predates
			// the counters. Every budget must work without preallocated maps.
			var budget RepassBudget
			gate := tc.gate
			gate.Name = "check"
			for attempt := 1; attempt <= tc.bound+1; attempt++ {
				want := RepassCharge{Bound: tc.bound, Exceeded: attempt > tc.bound}
				var wantBudget RepassBudget
				switch tc.outcome {
				case "timeout":
					want.Polling = true
					want.PollAttempt = attempt
					want.PollTarget = "retry"
					wantBudget.PollAttempts = map[string]int{"retry": attempt}
				case "infra":
					want.Infrastructure = true
					want.Attempt = attempt
					want.GateAttempt = attempt
					want.RepassTarget = "retry"
					wantBudget.Attempts = map[string]int{"check": 0}
					wantBudget.InfrastructureAttempts = map[string]int{"check": attempt}
					wantBudget.InfrastructureRepassAttempts = map[string]int{"retry": attempt}
				default:
					want.Attempt = attempt
					want.GateAttempt = attempt
					want.RepassTarget = "retry"
					wantBudget.Attempts = map[string]int{"check": attempt}
					wantBudget.InfrastructureAttempts = map[string]int{"check": 0}
					wantBudget.RepassAttempts = map[string]int{"retry": attempt}
				}
				if got := budget.Charge(gate, tc.outcome, "retry", true, tc.maxRepasses); got != want {
					t.Fatalf("charge %d = %+v, want %+v", attempt, got, want)
				}
				assertRepassBudget(t, budget, wantBudget)
			}
		})
	}
}

func TestRepassBudgetChargeOutcomes(t *testing.T) {
	tests := []struct {
		name             string
		outcome          string
		target           string
		reentry          bool
		withoutBranches  bool
		want             RepassCharge
		wantGatePolicy   int
		wantGateInfra    int
		wantTargetPolicy int
		wantTargetInfra  int
		wantTargetPolls  int
	}{
		{
			name: "passing forward edge clears consecutive counters", outcome: "pass", target: "implement",
			want: RepassCharge{Bound: 4}, wantTargetPolicy: 4,
		},
		{
			name: "passing reentry never spends repair budget", outcome: "pass", target: "implement", reentry: true,
			want: RepassCharge{Bound: 4}, wantTargetPolicy: 4,
		},
		{
			name: "forward failure advances only gate count", outcome: "fail", target: "implement",
			want: RepassCharge{GateAttempt: 3, Bound: 4}, wantGatePolicy: 3, wantTargetPolicy: 4,
		},
		{
			name: "review reentry exhausts seeded policy budget", outcome: string(apiv1.VerdictNeedsChanges), target: "implement", reentry: true,
			want:           RepassCharge{Attempt: 5, GateAttempt: 3, RepassTarget: "implement", Bound: 4, Exceeded: true},
			wantGatePolicy: 3, wantTargetPolicy: 5,
		},
		{
			name: "forward infrastructure failure advances only gate count", outcome: "infra", target: "local-ci",
			want:          RepassCharge{GateAttempt: 2, Infrastructure: true, Bound: 2},
			wantGateInfra: 2, wantTargetPolicy: 4, wantTargetInfra: 1,
		},
		{
			name: "infrastructure reentry spends its own budget", outcome: "infra", target: "local-ci", reentry: true,
			want:          RepassCharge{Attempt: 2, GateAttempt: 2, RepassTarget: "local-ci", Infrastructure: true, Bound: 2},
			wantGateInfra: 2, wantTargetPolicy: 4, wantTargetInfra: 2,
		},
		{
			name: "forward timeout preserves all counters", outcome: "timeout", target: "ci-poll",
			want:           RepassCharge{Bound: 4},
			wantGatePolicy: 2, wantGateInfra: 1, wantTargetPolicy: 4, wantTargetInfra: 1, wantTargetPolls: 2,
		},
		{
			name: "timeout reentry exhausts only polling budget", outcome: "timeout", target: "ci-poll", reentry: true,
			want:           RepassCharge{Bound: 2, Exceeded: true, Polling: true, PollAttempt: 3, PollTarget: "ci-poll"},
			wantGatePolicy: 2, wantGateInfra: 1, wantTargetPolicy: 4, wantTargetInfra: 1, wantTargetPolls: 3,
		},
		{
			name: "absent branch declarations preserve target counters", outcome: "fail", target: "implement", withoutBranches: true,
			want:           RepassCharge{GateAttempt: 3, Bound: 4},
			wantGatePolicy: 3, wantTargetPolicy: 4, wantTargetInfra: 1, wantTargetPolls: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gate := apiv1.Gate{
				Name:      "check",
				Automated: &apiv1.AutomatedGate{MaxTimeoutPolls: 2},
				Branches:  map[string]string{"infra": "local-ci", "timeout": "ci-poll"},
			}
			if tc.withoutBranches {
				gate.Branches = nil
			}
			// Resume counters deliberately differ by class and key. Resets must
			// use the declared branch, even when the outcome targets another stage.
			budget := RepassBudget{
				Attempts:                     map[string]int{"check": 2, "other-gate": 9},
				InfrastructureAttempts:       map[string]int{"check": 1, "other-gate": 9},
				RepassAttempts:               map[string]int{"implement": 4, "other-stage": 8},
				InfrastructureRepassAttempts: map[string]int{"local-ci": 1, "other-stage": 8},
				PollAttempts:                 map[string]int{"ci-poll": 2, "other-stage": 8},
			}
			if got := budget.Charge(gate, tc.outcome, tc.target, tc.reentry, 4); got != tc.want {
				t.Fatalf("Charge = %+v, want %+v", got, tc.want)
			}
			assertRepassBudget(t, budget, RepassBudget{
				Attempts:                     map[string]int{"check": tc.wantGatePolicy, "other-gate": 9},
				InfrastructureAttempts:       map[string]int{"check": tc.wantGateInfra, "other-gate": 9},
				RepassAttempts:               map[string]int{"implement": tc.wantTargetPolicy, "other-stage": 8},
				InfrastructureRepassAttempts: map[string]int{"local-ci": tc.wantTargetInfra, "other-stage": 8},
				PollAttempts:                 map[string]int{"ci-poll": tc.wantTargetPolls, "other-stage": 8},
			})
		})
	}
}

func TestRepassBudgetChargeSharesTargetAcrossGates(t *testing.T) {
	var budget RepassBudget
	steps := []struct {
		gate string
		want RepassCharge
	}{
		{gate: "review", want: RepassCharge{Attempt: 1, GateAttempt: 1, RepassTarget: "implement", Bound: 2}},
		{gate: "tests", want: RepassCharge{Attempt: 2, GateAttempt: 1, RepassTarget: "implement", Bound: 2}},
		{gate: "review", want: RepassCharge{Attempt: 3, GateAttempt: 2, RepassTarget: "implement", Bound: 2, Exceeded: true}},
	}
	for i, step := range steps {
		if got := budget.Charge(apiv1.Gate{Name: step.gate}, "fail", "implement", true, 2); got != step.want {
			t.Fatalf("step %d (%s): Charge = %+v, want %+v", i+1, step.gate, got, step.want)
		}
	}
	assertRepassBudget(t, budget, RepassBudget{
		Attempts:               map[string]int{"review": 2, "tests": 1},
		InfrastructureAttempts: map[string]int{"review": 0, "tests": 0},
		RepassAttempts:         map[string]int{"implement": 3},
	})
}

func TestRepassChargeEscalationReason(t *testing.T) {
	tests := []struct {
		name   string
		charge RepassCharge
		want   string
	}{
		{name: "zero value is policy", want: "REPASS_BUDGET_EXHAUSTED"},
		{name: "exhausted policy", charge: RepassCharge{Exceeded: true}, want: "REPASS_BUDGET_EXHAUSTED"},
		{name: "infrastructure", charge: RepassCharge{Infrastructure: true, Exceeded: true}, want: "INFRASTRUCTURE_REPASS_BUDGET_EXHAUSTED"},
		{name: "polling", charge: RepassCharge{Polling: true, Exceeded: true}, want: "POLLING_BUDGET_EXHAUSTED"},
		{name: "polling takes precedence", charge: RepassCharge{Infrastructure: true, Polling: true, Exceeded: true}, want: "POLLING_BUDGET_EXHAUSTED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.charge.EscalationReason(); got != tc.want {
				t.Fatalf("EscalationReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func assertRepassBudget(t *testing.T, got, want RepassBudget) {
	t.Helper()
	for _, counter := range []struct {
		name string
		got  map[string]int
		want map[string]int
	}{
		{name: "Attempts", got: got.Attempts, want: want.Attempts},
		{name: "InfrastructureAttempts", got: got.InfrastructureAttempts, want: want.InfrastructureAttempts},
		{name: "RepassAttempts", got: got.RepassAttempts, want: want.RepassAttempts},
		{name: "InfrastructureRepassAttempts", got: got.InfrastructureRepassAttempts, want: want.InfrastructureRepassAttempts},
		{name: "PollAttempts", got: got.PollAttempts, want: want.PollAttempts},
	} {
		if !maps.Equal(counter.got, counter.want) {
			t.Errorf("%s = %v, want %v", counter.name, counter.got, counter.want)
		}
	}
}
