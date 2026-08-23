package bandit

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func testConfig() Config {
	return Config{
		Stage: "review", Seed: 42, Arms: []Arm{
			{Name: "control", GateLevel: 2},
			{Name: "treatment", GateLevel: 2},
		},
		ExplorationBudget: 20, MinSamples: 2, MaxFailureRate: .75,
		MinLift: .1, Confidence: .8, TrainWindow: 4, EvalWindow: 4,
		DefaultGateLevel: 2,
	}
}

func TestAssignIsReplayableAndFillsSampleFloor(t *testing.T) {
	c := testConfig()
	first, err := c.Assign("run-1", nil)
	if err != nil {
		t.Fatal(err)
	}

	if first.Arm != "control" {
		t.Fatalf("first assignment = %q, want declaration-order control", first.Arm)
	}
	history := []Observation{
		{Stage: "review", Arm: "control", Success: true},
		{Stage: "review", Arm: "control", Success: false},
		{Stage: "review", Arm: "treatment", Success: true},
		{Stage: "review", Arm: "treatment", Success: false},
	}
	a, err := c.Assign("run-5", history)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Assign("run-5", history)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("replayed assignment = %+v and %+v", a, b)
	}
}

func TestAssignIgnoresEvalObservations(t *testing.T) {
	c := testConfig()
	history := []Observation{
		{Stage: "review", Arm: "control", Success: true, Window: "train"},
		{Stage: "review", Arm: "control", Success: false, Window: "train"},
		{Stage: "review", Arm: "treatment", Success: true, Window: "train"},
		{Stage: "review", Arm: "treatment", Success: false, Window: "train"},
	}
	withEval := append(append([]Observation(nil), history...),
		Observation{Stage: "review", Arm: "control", Success: false, Window: "eval"},
		Observation{Stage: "review", Arm: "treatment", Success: true, Window: "eval"},
	)
	withoutEval, err := c.Assign("run-5", history)
	if err != nil {
		t.Fatal(err)
	}
	withEvalAssignment, err := c.Assign("run-5", withEval)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withoutEval, withEvalAssignment) {
		t.Fatalf("eval observations changed assignment: %s vs %s", withoutEval.Arm, withEvalAssignment.Arm)
	}
}

func TestEvaluateRequiresPerArmSampleFloors(t *testing.T) {
	c := testConfig()
	var observations []Observation
	for i := 0; i < 4; i++ {
		observations = append(observations,
			Observation{Stage: "review", Arm: "control", Success: false, Window: "train"},
			Observation{Stage: "review", Arm: "treatment", Success: true, Window: "train"},
		)
	}
	observations = append(observations,
		Observation{Stage: "review", Arm: "control", Success: false, Window: "eval"},
		Observation{Stage: "review", Arm: "control", Success: false, Window: "eval"},
		Observation{Stage: "review", Arm: "control", Success: false, Window: "eval"},
		Observation{Stage: "review", Arm: "treatment", Success: true, Window: "eval"},
	)
	decision, err := c.Evaluate(observations)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Eligible || decision.Reason != "insufficient-arm-data" {
		t.Fatalf("decision = %+v, want per-arm sample-floor refusal", decision)
	}
}

type eventJournal struct {
	events []journal.Event
}

func (j *eventJournal) Append(event journal.Event) error {
	j.events = append(j.events, event)
	return nil
}

func TestAssignRecordApplyAndPromotionAreJournaled(t *testing.T) {
	c := testConfig()
	c.Arms[1].Variant = map[string]string{"harness": "claude"}
	j := new(eventJournal)
	assignment, err := c.AssignAndRecord("run-1", []Observation{
		{Stage: "review", Arm: "control"},
		{Stage: "review", Arm: "control"},
	}, j)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Apply(map[string]string{"timeout": "30s"})["harness"] != "claude" {
		t.Fatalf("assignment variant was not applied: %+v", assignment)
	}
	if len(j.events) != 1 || j.events[0].Type != journal.EventBanditAssignment {
		t.Fatalf("assignment events = %+v", j.events)
	}

	var observations []Observation
	for i := 0; i < 4; i++ {
		observations = append(observations,
			Observation{Stage: "review", Arm: "control", Success: false, Window: "train"},
			Observation{Stage: "review", Arm: "treatment", Success: true, Window: "train"},
			Observation{Stage: "review", Arm: "control", Success: false, Window: "eval"},
			Observation{Stage: "review", Arm: "treatment", Success: true, Window: "eval"},
		)
	}
	if err := c.RecordObservation(Observation{Stage: "review", RunID: "run-2", Arm: "treatment", Window: "eval"}, j); err != nil {
		t.Fatal(err)
	}
	decision, proposal, err := c.EvaluateAndRecord(observations, j)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Eligible || proposal == nil || !proposal.RequiresApproval {
		t.Fatalf("decision = %+v, proposal = %+v", decision, proposal)
	}
	if j.events[len(j.events)-1].Type != journal.EventBanditPromotionProposed {
		t.Fatalf("last event = %+v, want promotion proposal", j.events[len(j.events)-1])
	}
}

func TestAssignRejectsExhaustedBudget(t *testing.T) {
	c := testConfig()
	c.ExplorationBudget = 4
	_, err := c.Assign("run-2", []Observation{
		{Stage: "review", Arm: "control"},
		{Stage: "review", Arm: "control"},
		{Stage: "review", Arm: "treatment"},
		{Stage: "review", Arm: "treatment"},
	})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("error = %v, want budget exhaustion", err)
	}
}

func TestValidateRejectsGateWeakening(t *testing.T) {
	c := testConfig()
	c.Arms[1].GateLevel = 1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "weakens") {
		t.Fatalf("error = %v, want safety-arm rejection", err)
	}
}

func TestValidateUsesControlArmAsSafetyBaseline(t *testing.T) {
	c := testConfig()
	c.DefaultGateLevel = 1
	c.Arms[1].GateLevel = 1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "weakens") {
		t.Fatalf("error = %v, want control-arm safety baseline enforcement", err)
	}
}

func TestEvaluateUsesHeldOutWindowAndRetiresFailures(t *testing.T) {
	c := testConfig()
	var observations []Observation
	for i := 0; i < 4; i++ {
		observations = append(observations,
			Observation{Stage: "review", Arm: "control", Success: false, Window: "train"},
			Observation{Stage: "review", Arm: "treatment", Success: true, Window: "train"},
			Observation{Stage: "review", Arm: "control", Success: false, Window: "eval"},
			Observation{Stage: "review", Arm: "treatment", Success: true, Window: "eval"},
		)
	}
	decision, err := c.Evaluate(observations)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Eligible || decision.Arm != "treatment" || decision.EvalSamples != 8 {
		t.Fatalf("decision = %+v, want eligible held-out treatment", decision)
	}
	if !c.Retired("control", observations) {
		t.Fatal("control should be retired above failure-rate bound")
	}
	if c.Retired("treatment", observations) {
		t.Fatal("treatment should not be retired")
	}
}

func TestRetiredIgnoresPartialRewardOnSuccessfulObservation(t *testing.T) {
	c := testConfig()
	observations := []Observation{
		{
			Stage:     "review",
			Arm:       "treatment",
			Reward:    0.1,
			RewardSet: true,
			Success:   true,
			Window:    "eval",
		},
	}
	if c.Retired("treatment", observations) {
		t.Fatal("successful observation with partial reward should not retire the arm")
	}
}

func TestObservationMarshalAddsSchemaAndRejectsInvalidRecords(t *testing.T) {
	data, err := json.Marshal(Observation{Stage: "review", RunID: "run-1", Arm: "control", Window: "train"})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), `"schema":"`+Schema+`"`) {
		t.Fatalf("observation = %s, missing schema", data)
	}
	if _, err := json.Marshal(Observation{Stage: "review", RunID: "run-1", Arm: "control"}); err == nil {
		t.Fatal("invalid observation was marshaled")
	}
}

func TestExplicitZeroRewardIsPreserved(t *testing.T) {
	if got := observationReward(Observation{Reward: 0, RewardSet: true, Success: true}); got != 0 {
		t.Fatalf("explicit zero reward = %v, want 0", got)
	}
}
