package runner

import (
	"context"
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// Child wait annotations retain host custody without closing the stage.
const (
	ChildWaitKind      = journal.ChildWaitKind
	ChildContinuedKind = journal.ChildContinuedKind
	maxChildWaitBytes  = journal.MaxChildWaitBytes
)

// ChildHandoffRequest is an immutable host-verified queue/disposition receipt.
// It is never accepted from an invocation output or a tool-supplied assertion.
type ChildHandoffRequest journal.ChildHandoffRequest

// ChildWorkspaceCustody is transient host state, supplied after stop/join while
// the runner owns the checkout. Paths and provider credentials are not journaled.
type ChildWorkspaceCustody struct {
	Path    string
	RepoRef apiv1.RepoRef
}

// ChildPublicationStatus is a host-verified projection of a child's immutable
// publication custody. It contains no provider credentials or authored PR text.
type ChildPublicationStatus struct {
	SourceRunID       string `json:"sourceRunId"`
	Action            string `json:"action"`
	IntentDigest      string `json:"intentDigest"`
	State             string `json:"state"`
	Head              string `json:"head"`
	Base              string `json:"base"`
	Commit            string `json:"commit"`
	PullRequestURL    string `json:"pullRequestUrl"`
	PullRequestNumber int    `json:"pullRequestNumber"`
	NeedsHuman        bool   `json:"needsHuman"`
}

// ChildHandoffCompletion is verified by the host before it enters the parent's
// own bounded artifact. It grants no authority to read another run's journal.
type ChildHandoffCompletion struct {
	Publications     []ChildPublicationStatus `json:"publications,omitempty"`
	State            string                   `json:"state"`
	Summary          string                   `json:"summary"`
	ResultRef        string                   `json:"resultRef"`
	WorkspaceRef     string                   `json:"workspaceRef,omitempty"`
	References       []string                 `json:"references,omitempty"`
	DispositionIssue string                   `json:"dispositionIssue,omitempty"`
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

// ChildDispositionWaitError distinguishes an untouched failed preparation from
// a published plan whose effects must be reconciled before an agent resumes.
// Reason is bounded host-authored text, never raw Git or provider error output.
type ChildDispositionWaitError struct {
	Reason    string
	Reconcile bool
}

func (e *ChildDispositionWaitError) Error() string { return e.Reason }

// ChildParentSuspension reacquires the parent's existing concurrency permit.
type ChildParentSuspension interface{ Resume(context.Context) error }

// ChildCapacityWaitError identifies a scheduler refusal that can change while
// the parent remains parked. PolicyBlocked surfaces a durable operator-facing
// reason and uses a slower retry interval; unknown ownership errors are fatal.
type ChildCapacityWaitError struct {
	Reason        string
	PolicyBlocked bool
}

func (e *ChildCapacityWaitError) Error() string { return e.Reason }

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
	return journal.ChildHandoffRequest(r).Validate(origin)
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
// cannot replace the owner. Input must identify at most one waiting branch.
func pendingChildWait(events []journal.Event) (*childWaitRecord, journal.Event, error) {
	header, marker, err := journal.PendingChildWait(events)
	if err != nil || header == nil {
		return nil, marker, err
	}
	data, err := json.Marshal(marker.Runner["childWait"])
	if err != nil {
		return nil, marker, err
	}
	var record childWaitRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, marker, err
	}
	return &record, marker, nil
}

// ParkedOnChild reports durable, validated whole-parent suspension. Invalid
// custody is not presented as a safe capacity release; Resume reports its error.
func ParkedOnChild(events []journal.Event) bool {
	return journal.ParkedOnChild(events)
}

// ParkedChildRequestForOrigin selects one exact occurrence/attempt even when a
// sibling remains runnable. It grants only that branch's workspace custody.
func ParkedChildRequestForOrigin(events []journal.Event, origin apiv1.ChildWorkflowOrigin) (ChildHandoffRequest, journal.Event, bool, error) {
	projection, err := journal.ProjectChildWaits(events)
	if err != nil {
		return ChildHandoffRequest{}, journal.Event{}, false, err
	}
	for _, wait := range projection.Waits {
		if wait.Header.Request.Origin == origin {
			return ChildHandoffRequest(wait.Header.Request), wait.Started, true, nil
		}
	}
	return ChildHandoffRequest{}, journal.Event{}, false, nil
}

func decodeChildWaitEvent(event, started journal.Event) (*childWaitRecord, error) {
	if _, err := journal.DecodeChildWaitHeader(event, started); err != nil {
		return nil, err
	}
	data, err := json.Marshal(event.Runner["childWait"])
	if err != nil {
		return nil, err
	}
	var record childWaitRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	return &record, nil
}
