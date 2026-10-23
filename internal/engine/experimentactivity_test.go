package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/bandit"
)

type experimentTestPlane struct {
	history []bandit.Observation
	reads   int
	writes  []bandit.ObservationRecord
}

func (p *experimentTestPlane) Observe(_ context.Context, r bandit.ObservationRequest) (bandit.ObservationResponse, error) {
	if err := r.Validate(); err != nil {
		return bandit.ObservationResponse{}, err
	}
	if r.Operation == "read" {
		p.reads++
		return bandit.ObservationResponse{Observations: slices.Clone(p.history)}, nil
	}
	for _, prior := range p.writes {
		if prior.Key == r.Record.Key {
			if prior != *r.Record {
				return bandit.ObservationResponse{}, bandit.ErrObservationConflict
			}
			return bandit.ObservationResponse{}, nil
		}
	}
	p.writes = append(p.writes, *r.Record)
	p.history = append(p.history, r.Record.Observation)
	bandit.SortObservations(p.history)
	return bandit.ObservationResponse{}, nil
}

func experimentTestConfig() bandit.Config {
	return bandit.Config{
		Stage: "build", Seed: 42,
		Arms:              []bandit.Arm{{Name: "control", GateLevel: 1}, {Name: "treatment", GateLevel: 1, Variant: map[string]string{"choice": "treatment"}}},
		ExplorationBudget: 20000, MinSamples: 2, MaxFailureRate: .75,
		MinLift: .1, Confidence: .8, TrainWindow: 4, EvalWindow: 4, DefaultGateLevel: 1,
	}
}

func TestExperimentActivityPlanBoundsHistoryAndFreezesSampleSnapshot(t *testing.T) {
	plane := &experimentTestPlane{}
	for i := range 10000 {
		arm := "control"
		if i%2 == 1 {
			arm = "treatment"
		}
		plane.history = append(plane.history, bandit.Observation{Schema: bandit.Schema, RunID: fmt.Sprintf("past-%d", i), Stage: "build", Arm: arm, Window: "train", Reward: 1, RewardSet: true, Success: true})
	}
	activities := &Activities{Bandit: plane}
	plan, err := activities.PrepareExperiment(t.Context(), ExperimentPlanInput{RunID: "run-a", Config: experimentTestConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if plane.reads != 1 || len(plane.writes) != 0 {
		t.Fatalf("planning effects: reads=%d writes=%d", plane.reads, len(plane.writes))
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 4096 || bytes.Contains(raw, []byte("past-")) {
		t.Fatalf("plan persisted history instead of bounded decisions: %d bytes", len(raw))
	}
	var replayed ExperimentPlan
	if err := json.Unmarshal(raw, &replayed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(replayed.AssignmentEvents[0], []byte(fmt.Sprintf(`"seed":%d`, replayed.Assignment.Seed))) {
		t.Fatal("assignment seed lost integer precision in the history round trip")
	}
	// External observations can change after the assignment. Recording the
	// decided outcome must neither read again nor recalculate that decision.
	plane.history = nil
	record := bandit.ObservationRecord{Key: bandit.ObservationKey{RunID: "run-a", Stage: "build", Occurrence: 1, Attempt: 1}, Observation: replayed.Success.Observation}
	if err := activities.RecordExperiment(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if plane.reads != 1 || len(plane.writes) != 1 || plane.writes[0] != record {
		t.Fatalf("record recomputed outcome: reads=%d writes=%v", plane.reads, plane.writes)
	}
	if !replayed.Success.Observation.Success || replayed.Failure.Observation.Success || replayed.Success.Observation.Arm != replayed.Failure.Observation.Arm {
		t.Fatalf("outcomes don't share the recorded assignment: %+v", replayed)
	}
}

func TestExperimentActivitiesRequireObservationPlane(t *testing.T) {
	a := &Activities{}
	if _, err := a.PrepareExperiment(t.Context(), ExperimentPlanInput{RunID: "run-a", Config: experimentTestConfig()}); err == nil {
		t.Fatal("assignment bypassed missing observation plane")
	}
	if err := a.RecordExperiment(t.Context(), bandit.ObservationRecord{}); err == nil {
		t.Fatal("outcome bypassed missing observation plane")
	}
}
