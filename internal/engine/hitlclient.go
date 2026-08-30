package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// hitlclient.go is the daemon's half of the #3883 protocol: the one place a
// caller outside the workflow delivers an operator intent to an engine-driven
// run.
//
// It is deliberately thin. Every decision that can be made about an intent —
// is it well formed, is the actor entitled to it, is the run in a phase that
// can take it, is the terminal the operator saw still the current one — is
// made INSIDE the workflow, because the workflow is the only thing that knows.
// This side's job is to address the right execution, wait for the workflow's
// durable answer, and report exactly what it said.

// hitlUpdateTimeout bounds one delivery. It is generous relative to a
// workflow task (the handler blocks until the walk takes the intent up) and
// short relative to an operator's patience.
const hitlUpdateTimeout = 60 * time.Second

// hitlUpdateClient is the slice of client.Client this needs, so tests can
// substitute a fake without a Temporal server. client.Client satisfies it.
type hitlUpdateClient interface {
	UpdateWorkflow(ctx context.Context, options client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error)
}

// HITLDeliverer delivers operator intents to engine-driven runs.
type HITLDeliverer struct {
	client hitlUpdateClient
	// resolveWorkflowID maps a Goobers run id onto the Temporal workflow id it
	// executes under. A DIRECT engine run's workflow id IS its run id; a
	// SCHEDULED one's is not (RunScheduled hashes the claim workflow's id into
	// the run id), so without this an operator intent for a scheduled run is
	// addressed to a workflow that does not exist. nil falls back to the run
	// id, which is correct for every direct run.
	resolveWorkflowID func(runID string) (string, bool)
}

// NewHITLDeliverer builds a deliverer over a Temporal client.
func NewHITLDeliverer(c client.Client) (*HITLDeliverer, error) {
	if c == nil {
		return nil, errors.New("engine: Temporal client is required to deliver operator intents")
	}
	return &HITLDeliverer{client: c}, nil
}

// WithWorkflowIDResolver returns a deliverer that consults resolve before
// addressing a run. Returns d unchanged when either is nil.
func (d *HITLDeliverer) WithWorkflowIDResolver(resolve func(runID string) (string, bool)) *HITLDeliverer {
	if d == nil || resolve == nil {
		return d
	}
	next := *d
	next.resolveWorkflowID = resolve
	return &next
}

// ErrHITLRunNotFound is returned when no workflow is addressable for the run.
// The daemon turns it into a 404 rather than a refusal: there is nothing to
// deliver to, which is a different fact from "the run refused this".
var ErrHITLRunNotFound = errors.New("engine: no engine workflow is addressable for this run")

// Deliver submits one operator intent and returns the workflow's own answer.
//
// It reports success ONLY once the update has reached
// WorkflowUpdateStageCompleted and handle.Get has returned the workflow's ack.
// That is the whole point of using an Update rather than a Signal: at the
// moment this function returns nil, the run has durably recorded the
// intervention in its history and (for anything but a deny) has actually
// resumed. There is no window in which a caller has been told "accepted" and
// the run has not accepted.
func (d *HITLDeliverer) Deliver(ctx context.Context, intent HITLIntent) (HITLAck, error) {
	if d == nil || d.client == nil {
		return HITLAck{}, errors.New("engine: no Temporal client is configured for operator intents")
	}
	intent.Protocol = HITLProtocol
	intent.Version = HITLProtocolVersion
	runID := strings.TrimSpace(intent.RunID)
	if runID == "" {
		return HITLAck{}, errors.New("engine: operator intent names no run")
	}
	if strings.TrimSpace(intent.RequestID) == "" {
		return HITLAck{}, errors.New("engine: operator intent carries no request id")
	}
	workflowID := runID
	if d.resolveWorkflowID != nil {
		if resolved, ok := d.resolveWorkflowID(runID); ok && resolved != "" {
			workflowID = resolved
		}
	}

	ctx, cancel := context.WithTimeout(ctx, hitlUpdateTimeout)
	defer cancel()

	handle, err := d.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		// The request id IS the Temporal update id, so a retried delivery is
		// deduplicated by the SERVER before it ever reaches the workflow. The
		// workflow deduplicates again against its own record, which is what
		// catches a key reused for a different payload.
		UpdateID:   intent.RequestID,
		WorkflowID: workflowID,
		UpdateName: HITLUpdateName,
		Args:       []interface{}{intent},
		// Completed, never Accepted: an accepted-but-unfinished update means
		// the workflow took the request, not that it took the DECISION.
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		if isHITLNotFound(err) {
			return HITLAck{}, fmt.Errorf("%w: %s", ErrHITLRunNotFound, runID)
		}
		return HITLAck{}, fmt.Errorf("engine: deliver operator intent to run %s: %w", runID, err)
	}
	var ack HITLAck
	if err := handle.Get(ctx, &ack); err != nil {
		if isHITLNotFound(err) {
			return HITLAck{}, fmt.Errorf("%w: %s", ErrHITLRunNotFound, runID)
		}
		// A protocol refusal is the workflow's considered answer, and is
		// returned unwrapped in meaning: HITLRefusalCode reads its code and
		// the message is the operator-facing sentence the workflow wrote.
		return HITLAck{}, err
	}
	return ack, nil
}

func isHITLNotFound(err error) bool {
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}
