package journal

import (
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
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
	if origin == nil || r.Origin != *origin || r.Gaggle == "" || len(r.Gaggle) > 128 || !apiv1.ValidRunID(r.ParentRunID) || len(r.ParentRunID) > 256 || !blobstore.ValidDigest(r.RequestID) || !blobstore.ValidDigest(r.SourceDigest) || !apiv1.ValidRunID(r.ChildRunID) || len(r.ChildRunID) > 256 || r.AcceptanceID != "trigger-"+r.ChildRunID || r.InvocationKey == "" || len(r.InvocationKey) > 256 {
		return fmt.Errorf("runner: child handoff receipt does not match the active invocation")
	}
	if r.DispositionDigest != "" && !blobstore.ValidDigest(r.DispositionDigest) {
		return fmt.Errorf("runner: invalid child disposition revision")
	}
	switch r.Action {
	case "wait", "merge", "replace", "discard":
		return nil
	default:
		return fmt.Errorf("runner: unsupported child handoff action")
	}
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

// PendingChildWait returns a single pending branch for callers with a filtered
// branch history. Callers owning a whole parallel must select an exact branch.
func PendingChildWait(events []Event) (*ChildWaitHeader, Event, error) {
	projection, err := ProjectChildWaits(events)
	if err != nil {
		return nil, Event{}, err
	}
	if len(projection.Waits) > 1 {
		return nil, Event{}, fmt.Errorf("runner: child wait requires an exact branch")
	}
	for _, wait := range projection.Waits {
		return &wait.Header, wait.Marker, nil
	}
	return nil, Event{}, nil
}

// PendingChildWaitForBranch selects one branch without borrowing sibling custody.
func PendingChildWaitForBranch(events []Event, branch int) (*ChildWaitHeader, Event, error) {
	projection, err := ProjectChildWaits(events)
	if err != nil {
		return nil, Event{}, err
	}
	wait, found := projection.Waits[branch]
	if !found {
		return nil, Event{}, nil
	}
	return &wait.Header, wait.Marker, nil
}

// ParkedOnChild releases capacity only when every declared unfinished branch
// waits. Queued siblings and gaps between stages remain runnable.
func ParkedOnChild(events []Event) bool {
	projection, err := ProjectChildWaits(events)
	return err == nil && projection.Parked()
}

// DecodeChildWaitHeader verifies a bounded wait against its durable started event.
func DecodeChildWaitHeader(event, started Event) (*ChildWaitHeader, error) {
	data, err := json.Marshal(event.Runner["childWait"])
	if err != nil || len(data) > MaxChildWaitBytes {
		return nil, fmt.Errorf("runner: invalid child wait record")
	}
	var record ChildWaitHeader
	if err := json.Unmarshal(data, &record); err != nil || record.Version != 1 || event.Stage != started.Stage || event.Attempt != started.Attempt || event.Branch != started.Branch || record.PolicyAttempts < 0 || record.InfrastructureFailures < 0 || record.ParentRunID != record.Request.ParentRunID {
		return nil, fmt.Errorf("runner: child wait is not bound to its branch stage attempt")
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
