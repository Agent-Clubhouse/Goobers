package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/journal"
)

const (
	actPrepareExperiment = "PrepareExperiment"
	actRecordExperiment  = "RecordExperiment"
)

// ExperimentPlanInput contains only pinned task policy and current-run dedup
// state. The local activity fetches the sample history through the plane.
type ExperimentPlanInput struct {
	RunID        string
	Config       bandit.Config
	PriorRetired []string
}

// ExperimentOutcome contains one possible binary result and its journal events.
// Raw JSON preserves the shared journal encoder's exact numeric representation.
type ExperimentOutcome struct {
	Observation bandit.Observation
	Events      []json.RawMessage
}

// ExperimentPlan is the local activity result recorded in Temporal history.
// Its size depends on declared arms, not the number of historical observations.
// Both outcomes use the assignment's sample snapshot, just like the runner.
type ExperimentPlan struct {
	Assignment       bandit.Assignment
	AssignmentEvents []json.RawMessage
	Success, Failure ExperimentOutcome
}

// PrepareExperiment assigns an arm and prepares both possible outcomes from one
// read. It has no write side effects; Temporal records the resulting plan before
// the workflow emits an assignment or dispatches a stage.
func (a *Activities) PrepareExperiment(ctx context.Context, input ExperimentPlanInput) (ExperimentPlan, error) {
	if a.Bandit == nil {
		return ExperimentPlan{}, errors.New("engine experiment observation plane is not configured")
	}
	if err := input.Config.Validate(); err != nil {
		return ExperimentPlan{}, err
	}
	response, err := a.Bandit.Observe(ctx, bandit.ObservationRequest{Operation: "read", RunID: input.RunID, Stage: input.Config.Stage})
	if err != nil {
		return ExperimentPlan{}, err
	}
	buffer := &experimentEventBuffer{}
	assignment, err := input.Config.AssignAndRecord(input.RunID, response.Observations, buffer)
	if err != nil {
		return ExperimentPlan{}, err
	}
	plan := ExperimentPlan{Assignment: assignment, AssignmentEvents: buffer.events}
	window := bandit.ObservationWindow(input.Config, response.Observations)
	plan.Success, err = prepareExperimentOutcome(input, assignment, window, response.Observations, apiv1.ResultSuccess)
	if err != nil {
		return ExperimentPlan{}, err
	}
	plan.Failure, err = prepareExperimentOutcome(input, assignment, window, response.Observations, apiv1.ResultFailure)
	if err != nil {
		return ExperimentPlan{}, err
	}
	return plan, nil
}

func prepareExperimentOutcome(input ExperimentPlanInput, assignment bandit.Assignment, window string, history []bandit.Observation, status apiv1.ResultStatus) (ExperimentOutcome, error) {
	buffer := &experimentEventBuffer{}
	for _, arm := range input.PriorRetired {
		buffer.prior = append(buffer.prior, journal.Event{Type: journal.EventBanditRetired, Stage: input.Config.Stage, Outputs: map[string]any{"arm": arm}})
	}
	if err := bandit.RecordResult(input.Config, input.RunID, assignment, window, history, apiv1.ResultEnvelope{Status: status}, buffer); err != nil {
		return ExperimentOutcome{}, err
	}
	for _, raw := range buffer.events {
		var event journal.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return ExperimentOutcome{}, err
		}
		if event.Type == journal.EventBanditObservation {
			payload, err := json.Marshal(event.Outputs)
			if err != nil {
				return ExperimentOutcome{}, err
			}
			var observation bandit.Observation
			if err := json.Unmarshal(payload, &observation); err != nil {
				return ExperimentOutcome{}, err
			}
			return ExperimentOutcome{Observation: observation, Events: buffer.events}, nil
		}
	}
	return ExperimentOutcome{}, errors.New("shared bandit result did not produce an observation")
}

// RecordExperiment publishes the already-decided outcome. Exact attempt
// identity makes a response lost after commit safe to retry on another worker.
func (a *Activities) RecordExperiment(ctx context.Context, record bandit.ObservationRecord) error {
	if a.Bandit == nil {
		return errors.New("engine experiment observation plane is not configured")
	}
	_, err := a.Bandit.Observe(ctx, bandit.ObservationRequest{Operation: "record", RunID: record.Key.RunID, Stage: record.Key.Stage, Record: &record})
	return err
}

type experimentEventBuffer struct {
	prior  []journal.Event
	events []json.RawMessage
}

func (b *experimentEventBuffer) Append(event journal.Event) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode experiment event: %w", err)
	}
	b.prior = append(b.prior, event)
	b.events = append(b.events, raw)
	return nil
}

func (b *experimentEventBuffer) AppendIfAbsent(event journal.Event, matches func(journal.Event) bool) (bool, error) {
	for _, prior := range b.prior {
		if matches(prior) {
			return false, nil
		}
	}
	return true, b.Append(event)
}
