package runner

import (
	"encoding/json"
	"errors"
	"maps"
	"math/big"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

const childAttemptAccountingKind = "child.workflow.attempt-accounting"

type childAttemptAccounting struct {
	Origin                 apiv1.ChildWorkflowOrigin `json:"origin"`
	PolicyAttempts         int32                     `json:"policyAttempts"`
	InfrastructureFailures int32                     `json:"infrastructureFailures"`
	Usage                  map[string]float64        `json:"usage,omitempty"`
	CostUSD                string                    `json:"costUsd,omitempty"`
	InvalidCost            bool                      `json:"invalidCost,omitempty"`
}

func (tf *taskFrame) recordTaskStartedWithRecovery(attempt int, class journal.AttemptClass, policy, infra int32, total *stageUsageTotals) error {
	if err := tf.recordTaskStarted(attempt, class); err != nil {
		return err
	}
	if tf.childOrigin == nil {
		return nil
	}
	record := childAttemptAccounting{Origin: *tf.childOrigin, PolicyAttempts: policy, InfrastructureFailures: infra, Usage: maps.Clone(total.metrics), InvalidCost: total.invalidCost}
	if total.costUSD != nil {
		record.CostUSD = total.costUSD.RatString()
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) > 4096 {
		return errors.New("child attempt accounting exceeds bound")
	}
	return tf.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: tf.t.Name, Attempt: attempt, AttemptClass: class, Runner: map[string]any{"kind": childAttemptAccountingKind, "accounting": record}})
}

// RecoverContainedParentWait restores acceptance that committed before the host
// could publish its wait. The original attempt's allowance and supervised usage
// are retained; no attempt, grant, or worker is created. A terminal parent is
// left terminal until its existing authorized continuation reopens it.
func RecoverContainedParentWait(writer interface {
	Append(journal.Event) error
	Dir() string
}, request ChildHandoffRequest) error {
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		return err
	}
	phase, err := reader.Phase()
	if err != nil || phase != journal.PhaseRunning {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	pending, _, waiting, err := ParkedChildRequestForOrigin(events, request.Origin)
	if err != nil {
		return err
	}
	if waiting {
		if pending != request {
			return errors.New("recovered wait differs from pending child")
		}
		return nil
	}
	record, started, err := recoverAcceptedWait(reader, events, request)
	if err != nil {
		return err
	}
	event, err := childWaitEvent(started.Stage, started.Attempt, started.AttemptClass, record)
	if err != nil {
		return err
	}
	event.Branch = started.Branch
	return writer.Append(event)
}

func recoverAcceptedWait(reader *journal.Reader, events []journal.Event, request ChildHandoffRequest) (childWaitRecord, journal.Event, error) {
	var recovered *containedParentRecovery
	var accounting *childAttemptAccounting
	var started journal.Event
	for _, event := range events {
		var err error
		started, err = acceptedWaitStart(event, request, started)
		if err != nil {
			return childWaitRecord{}, started, err
		}
		if event.Type != journal.EventRunnerAnnotation || started.Seq == 0 || event.Stage != started.Stage || event.Attempt != started.Attempt || event.Branch != started.Branch {
			continue
		}
		switch event.Runner["kind"] {
		case childAttemptAccountingKind:
			value, err := readChildAttemptAccounting(event, request.Origin, accounting != nil)
			if err != nil {
				return childWaitRecord{}, started, err
			}
			accounting = value
		case ContainedParentRecoveredKind:
			value, err := readAcceptedParentRecovery(reader, event, request, recovered != nil)
			if err != nil {
				return childWaitRecord{}, started, err
			}
			recovered = value
		}
	}
	if recovered == nil || accounting == nil || started.Seq == 0 {
		return childWaitRecord{}, started, errors.New("accepted child lacks original attempt recovery custody")
	}
	if err := request.validate(recovered.Custody.Origin); err != nil {
		return childWaitRecord{}, started, err
	}
	usage, err := restoreChildAccounting(*accounting, recovered.Usage)
	if err != nil {
		return childWaitRecord{}, started, err
	}
	record := childWaitRecord{Version: 1, ParentRunID: request.ParentRunID, Request: request, Workspace: &recovered.Custody.Workspace, Context: recovered.Context, Transcript: recovered.Transcript, InstructionAddendum: recovered.InstructionAddendum, PolicyAttempts: accounting.PolicyAttempts, InfrastructureFailures: accounting.InfrastructureFailures, Usage: usage.metrics, InvalidCost: usage.invalidCost}
	if usage.costUSD != nil {
		record.CostUSD = usage.costUSD.RatString()
	}
	return record, started, nil
}

func acceptedWaitStart(event journal.Event, request ChildHandoffRequest, previous journal.Event) (journal.Event, error) {
	if event.Type != journal.EventStageStarted {
		return previous, nil
	}
	origin, err := journal.ChildWorkflowOriginForEvent(request.ParentRunID, event)
	if err == nil && *origin == request.Origin {
		return event, nil
	}
	if previous.Seq != 0 && event.Seq > previous.Seq && event.Branch == previous.Branch {
		return previous, errors.New("recovered wait has a newer stage owner")
	}
	return previous, nil
}

func restoreChildAccounting(record childAttemptAccounting, observed map[string]float64) (*stageUsageTotals, error) {
	usage := newStageUsageTotals()
	usage.metrics, usage.invalidCost = maps.Clone(record.Usage), record.InvalidCost
	if usage.metrics == nil {
		usage.metrics = map[string]float64{}
	}
	if record.CostUSD != "" {
		var ok bool
		usage.costUSD, ok = new(big.Rat).SetString(record.CostUSD)
		if !ok || usage.costUSD.Sign() < 0 {
			return nil, errors.New("invalid retained child cost")
		}
	}
	accumulateStageUsage(usage, observed)
	return usage, nil
}

func readChildAttemptAccounting(event journal.Event, origin apiv1.ChildWorkflowOrigin, duplicate bool) (*childAttemptAccounting, error) {
	var value childAttemptAccounting
	data, _ := json.Marshal(event.Runner["accounting"])
	if duplicate || len(data) > 4096 || json.Unmarshal(data, &value) != nil || value.Origin != origin || value.PolicyAttempts < 0 || value.InfrastructureFailures < 0 {
		return nil, errors.New("invalid retained child attempt accounting")
	}
	return &value, nil
}

func readAcceptedParentRecovery(reader *journal.Reader, event journal.Event, request ChildHandoffRequest, duplicate bool) (*containedParentRecovery, error) {
	value, err := readParentRecovery(reader, event, request.ParentRunID)
	if err != nil || value.Custody.Origin == nil || *value.Custody.Origin != request.Origin || duplicate {
		return nil, errors.Join(errors.New("invalid recovered parent wait custody"), err)
	}
	return &value, nil
}
