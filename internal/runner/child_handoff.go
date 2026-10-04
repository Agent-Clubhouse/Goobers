package runner

import (
	"context"
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// Child wait annotations retain host custody without closing the stage.
const (
	ChildWaitKind      = "child.workflow.wait"
	ChildContinuedKind = "child.workflow.continued"
	maxChildWaitBytes  = 64 << 10
)

// ChildHandoffRequest is an immutable host-verified queue/disposition receipt.
// It is never accepted from an invocation output or a tool-supplied assertion.
type ChildHandoffRequest struct {
	Gaggle        string                    `json:"gaggle"`
	ParentRunID   string                    `json:"parentRunId"`
	RequestID     string                    `json:"requestId"`
	Action        string                    `json:"action"`
	ChildRunID    string                    `json:"childRunId"`
	AcceptanceID  string                    `json:"acceptanceId"`
	InvocationKey string                    `json:"invocationKey"`
	SourceDigest  string                    `json:"sourceDigest"`
	Origin        apiv1.ChildWorkflowOrigin `json:"origin"`
}

// ChildWorkspaceCustody is transient host state, supplied after stop/join while
// the runner owns the checkout. Paths and provider credentials are not journaled.
type ChildWorkspaceCustody struct {
	Path    string
	RepoRef apiv1.RepoRef
}

// ChildHandoffCompletion is verified by the host before it enters the parent's
// own bounded artifact. It grants no authority to read another run's journal.
type ChildHandoffCompletion struct {
	State        string   `json:"state"`
	Summary      string   `json:"summary"`
	ResultRef    string   `json:"resultRef"`
	WorkspaceRef string   `json:"workspaceRef,omitempty"`
	References   []string `json:"references,omitempty"`
}

// ChildHandoff owns queue authority and custody effects. Await observes a
// durable accepted request; it is NOT evidence of workspace quiescence. Yield
// runs only after the runner has joined writers and committed its wait marker.
// Wait may block indefinitely. Implementations must honor cancellation.
type ChildHandoff interface {
	Await(context.Context, apiv1.InvocationEnvelope) (ChildHandoffRequest, error)
	Yield(context.Context, ChildHandoffRequest, ChildWorkspaceCustody) error
	Wait(context.Context, ChildHandoffRequest) (ChildHandoffCompletion, error)
}

// ChildParentSuspension reacquires the parent's existing concurrency permit.
type ChildParentSuspension interface{ Resume(context.Context) error }

// ChildParentCapacity releases only concurrency, retaining the accepted start
// and budget. The initial runner integration supports serial parent graphs.
type ChildParentCapacity interface {
	SuspendChildParent(context.Context, string) (ChildParentSuspension, error)
}

type childWaitRecord struct {
	Version                int                    `json:"version"`
	ParentRunID            string                 `json:"parentRunId"`
	Request                ChildHandoffRequest    `json:"request"`
	Workspace              *worktree.StageCustody `json:"workspace,omitempty"`
	Context                []apiv1.ContextPointer `json:"context,omitempty"`
	Transcript             *apiv1.ArtifactPointer `json:"transcript,omitempty"`
	InstructionAddendum    string                 `json:"instructionAddendum,omitempty"`
	PolicyAttempts         int32                  `json:"policyAttempts"`
	InfrastructureFailures int32                  `json:"infrastructureFailures"`
	Usage                  map[string]float64     `json:"usage,omitempty"`
	CostUSD                string                 `json:"costUsd,omitempty"`
	InvalidCost            bool                   `json:"invalidCost,omitempty"`
}

func (r ChildHandoffRequest) validate(origin *apiv1.ChildWorkflowOrigin) error {
	if origin == nil || r.Origin != *origin || r.Gaggle == "" || len(r.Gaggle) > 128 || !apiv1.ValidRunID(r.ParentRunID) || len(r.ParentRunID) > 256 || !blobstore.ValidDigest(r.RequestID) || !blobstore.ValidDigest(r.SourceDigest) || !apiv1.ValidRunID(r.ChildRunID) || len(r.ChildRunID) > 256 || r.AcceptanceID != "trigger-"+r.ChildRunID || r.InvocationKey == "" || len(r.InvocationKey) > 256 {
		return fmt.Errorf("runner: child handoff receipt does not match the active invocation")
	}
	switch r.Action {
	case "wait", "merge", "replace", "discard":
		return nil
	default:
		return fmt.Errorf("runner: unsupported child handoff action")
	}
}

func childWaitEvent(stage string, attempt int, class journal.AttemptClass, record childWaitRecord) (journal.Event, error) {
	data, err := json.Marshal(record)
	if err != nil || len(data) > maxChildWaitBytes {
		return journal.Event{}, fmt.Errorf("runner: child wait exceeds its durable record bound")
	}
	return journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: attempt, AttemptClass: class, Runner: map[string]any{"kind": ChildWaitKind, "childWait": record}}, nil
}

// pendingChildWait validates the marker against the actual durable started
// event. Observational events do not unpark a parent, and a sibling's marker
// cannot replace the owner. Parallel parking is deliberately not implemented.
func pendingChildWait(events []journal.Event) (*childWaitRecord, journal.Event, error) {
	var pending *childWaitRecord
	var started, marker journal.Event
	for _, event := range events {
		switch event.Type {
		case journal.EventStageStarted:
			if pending != nil {
				return nil, marker, fmt.Errorf("runner: child wait has an unacknowledged replacement attempt")
			}
			started = event
		case journal.EventStageFinished, journal.EventRunFinished:
			pending = nil
		case journal.EventRunnerAnnotation:
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
			record, err := decodeChildWaitEvent(event, started)
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

// ParkedOnChild reports durable, validated serial parent suspension. Invalid
// custody is not presented as a safe capacity release; Resume reports its error.
func ParkedOnChild(events []journal.Event) bool {
	pending, _, err := pendingChildWait(events)
	return err == nil && pending != nil
}

func decodeChildWaitEvent(event, started journal.Event) (*childWaitRecord, error) {
	data, err := json.Marshal(event.Runner["childWait"])
	if err != nil || len(data) > maxChildWaitBytes {
		return nil, fmt.Errorf("runner: invalid child wait record")
	}
	var record childWaitRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Version != 1 || event.Stage != started.Stage || event.Attempt != started.Attempt || event.Branch != 0 || record.PolicyAttempts < 0 || record.InfrastructureFailures < 0 || record.ParentRunID != record.Request.ParentRunID {
		return nil, fmt.Errorf("runner: child wait is not bound to a serial stage attempt")
	}
	origin, err := journal.ChildWorkflowOriginForEvent(record.ParentRunID, started)
	if err != nil {
		return nil, err
	}
	if err := record.Request.validate(origin); err != nil {
		return nil, err
	}
	return &record, nil
}
