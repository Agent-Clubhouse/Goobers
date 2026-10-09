package journal

import (
	"encoding/json"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ChildWaitKind marks durable host custody independent of runner implementation.
const ChildWaitKind = "child.workflow.wait"

// ChildContinuedKind closes a matching wait after capacity is reacquired.
const ChildContinuedKind = "child.workflow.continued"

// MaxChildWaitBytes bounds each retained wait annotation.
const MaxChildWaitBytes = 64 << 10

// ChildHandoffRequest binds an accepted child receipt to its actual stage origin.
type ChildHandoffRequest struct {
	Gaggle            string                    `json:"gaggle"`
	ParentRunID       string                    `json:"parentRunId"`
	RequestID         string                    `json:"requestId"`
	Action            string                    `json:"action"`
	DispositionDigest string                    `json:"dispositionDigest,omitempty"`
	ChildRunID        string                    `json:"childRunId"`
	AcceptanceID      string                    `json:"acceptanceId"`
	InvocationKey     string                    `json:"invocationKey"`
	SourceDigest      string                    `json:"sourceDigest"`
	Origin            apiv1.ChildWorkflowOrigin `json:"origin"`
}

// Validate binds the receipt to a trusted current stage occurrence.
func (r ChildHandoffRequest) Validate(origin *apiv1.ChildWorkflowOrigin) error {
	if origin == nil || r.Origin != *origin || r.Gaggle == "" || len(r.Gaggle) > 128 || !apiv1.ValidRunID(r.ParentRunID) || len(r.ParentRunID) > 256 || !validChildDigest(r.RequestID) || !validChildDigest(r.SourceDigest) || !apiv1.ValidRunID(r.ChildRunID) || len(r.ChildRunID) > 256 || r.AcceptanceID != "trigger-"+r.ChildRunID || r.InvocationKey == "" || len(r.InvocationKey) > 256 {
		return fmt.Errorf("runner: child handoff receipt does not match the active invocation")
	}
	if r.DispositionDigest != "" && !validChildDigest(r.DispositionDigest) {
		return fmt.Errorf("runner: invalid child disposition revision")
	}
	switch r.Action {
	case "wait", "merge", "replace", "discard":
		return nil
	default:
		return fmt.Errorf("runner: unsupported child handoff action")
	}
}

// Keep journal validation independent of the blob storage implementation while
// preserving canonical lowercase receipt identities.
func validChildDigest(value string) bool {
	_, err := digestHex(value)
	return err == nil && value == strings.ToLower(value)
}

// ChildWaitHeader is the bounded authority projection shared by scheduling and
// execution. Workspace/context payloads remain runner-owned; they grant no slot.
type ChildWaitHeader struct {
	Version                int                 `json:"version"`
	ParentRunID            string              `json:"parentRunId"`
	Request                ChildHandoffRequest `json:"request"`
	PolicyAttempts         int32               `json:"policyAttempts"`
	InfrastructureFailures int32               `json:"infrastructureFailures"`
}

// PendingChildWait projects the latest valid serial suspension from host events.
func PendingChildWait(events []Event) (*ChildWaitHeader, Event, error) {
	var pending *ChildWaitHeader
	var started, marker Event
	for _, event := range events {
		switch event.Type {
		case EventStageStarted:
			if pending != nil {
				return nil, marker, fmt.Errorf("runner: child wait has an unacknowledged replacement attempt")
			}
			started = event
		case EventStageFinished, EventRunFinished:
			pending = nil
		case EventRunnerAnnotation:
			kind, _ := event.Runner["kind"].(string)
			if kind == ChildContinuedKind && pending != nil {
				if event.Runner["requestId"] != pending.Request.RequestID || event.Stage != marker.Stage || event.Attempt != marker.Attempt {
					return nil, marker, fmt.Errorf("runner: child continuation does not match its wait")
				}
				pending = nil
			}
			if kind != ChildWaitKind {
				continue
			}
			record, err := DecodeChildWaitHeader(event, started)
			if err != nil {
				return nil, marker, err
			}
			if pending != nil && pending.Request.RequestID != record.Request.RequestID {
				return nil, marker, fmt.Errorf("runner: overlapping child waits")
			}
			pending, marker = record, event
		}
	}
	return pending, marker, nil
}

// ParkedOnChild releases no capacity for a malformed or unmatched wait marker.
func ParkedOnChild(events []Event) bool {
	pending, _, err := PendingChildWait(events)
	return err == nil && pending != nil
}

// DecodeChildWaitHeader verifies a bounded wait against its durable started event.
func DecodeChildWaitHeader(event, started Event) (*ChildWaitHeader, error) {
	if event.Branch != 0 {
		return nil, fmt.Errorf("runner: child wait is not bound to a serial stage attempt")
	}
	return decodeBoundChildWaitHeader(event, started)
}

// decodeBoundChildWaitHeader also supports read-only projection of branch waits.
// Runtime admission retains its existing serial-only boundary above.
func decodeBoundChildWaitHeader(event, started Event) (*ChildWaitHeader, error) {
	data, err := json.Marshal(event.Runner["childWait"])
	if err != nil || len(data) > MaxChildWaitBytes {
		return nil, fmt.Errorf("runner: invalid child wait record")
	}
	var record ChildWaitHeader
	if err := json.Unmarshal(data, &record); err != nil || record.Version != 1 || event.Stage != started.Stage || event.Attempt != started.Attempt || event.Branch != started.Branch || record.PolicyAttempts < 0 || record.InfrastructureFailures < 0 || record.ParentRunID != record.Request.ParentRunID {
		return nil, fmt.Errorf("runner: child wait is not bound to a serial stage attempt")
	}
	origin, err := ChildWorkflowOriginForEvent(record.ParentRunID, started)
	if err != nil {
		return nil, err
	}
	if err := record.Request.Validate(origin); err != nil {
		return nil, err
	}
	return &record, nil
}
