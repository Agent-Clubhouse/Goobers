package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// hitlclient_test.go pins how an operator intent is ADDRESSED, which is the one
// decision the daemon side makes on its own.
//
// It matters more than it looks. A scheduled engine run's workflow id is not
// its run id (RunScheduled hashes the claim workflow's id in), so an intent
// addressed by run id alone comes back NotFound for a run that is very much
// executing and holding a scheduler slot. #3877 built the inverse lookup; this
// is the protocol consuming it, and consuming it with the SAME three-way
// reading of its errors, because the wrong reading tells an operator their run
// does not exist when the truth is that the daemon could not tell.

// hitlFakeHandle is a WorkflowUpdateHandle whose Get answers from a fixture.
type hitlFakeHandle struct {
	workflowID string
	ack        HITLAck
	err        error
}

func (h *hitlFakeHandle) WorkflowID() string { return h.workflowID }
func (h *hitlFakeHandle) RunID() string      { return "" }
func (h *hitlFakeHandle) UpdateID() string   { return "" }

func (h *hitlFakeHandle) Get(_ context.Context, valuePtr interface{}) error {
	if h.err != nil {
		return h.err
	}
	if out, ok := valuePtr.(*HITLAck); ok {
		*out = h.ack
	}
	return nil
}

// hitlFakeUpdateClient answers UpdateWorkflow from a table keyed by workflow
// id, recording every id it was asked for in order.
type hitlFakeUpdateClient struct {
	known     map[string]HITLAck
	getErr    map[string]error
	addressed []string
}

func (c *hitlFakeUpdateClient) UpdateWorkflow(_ context.Context, options client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error) {
	c.addressed = append(c.addressed, options.WorkflowID)
	ack, ok := c.known[options.WorkflowID]
	if !ok {
		return nil, serviceerror.NewNotFound("workflow not found")
	}
	return &hitlFakeHandle{workflowID: options.WorkflowID, ack: ack, err: c.getErr[options.WorkflowID]}, nil
}

func hitlDelivererOver(c hitlUpdateClient) *HITLDeliverer {
	return &HITLDeliverer{client: c}
}

func hitlTestIntent() HITLIntent {
	return HITLIntent{
		RunID:                      "run-1",
		RequestID:                  "req-1",
		Actor:                      "operator",
		Kind:                       HITLResolveEscalation,
		Resolution:                 HITLResolutionApprove,
		Gate:                       "review",
		Target:                     "ship",
		ExpectedTerminalGeneration: 1,
	}
}

// A direct run is addressed by its run id and NOTHING else is consulted: the
// inverse lookup is an enumeration, and the common case must not pay for it.
func TestHITLDelivererAddressesTheRunIDFirst(t *testing.T) {
	fake := &hitlFakeUpdateClient{known: map[string]HITLAck{"run-1": {RequestID: "req-1"}}}
	resolved := 0
	d := hitlDelivererOver(fake).WithWorkflowIDResolver(func(context.Context, string) (string, error) {
		resolved++
		return "other", nil
	})

	ack, err := d.Deliver(context.Background(), hitlTestIntent())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if ack.RequestID != "req-1" {
		t.Fatalf("ack.RequestID = %q, want the intent's key echoed back", ack.RequestID)
	}
	if resolved != 0 {
		t.Fatalf("the resolver was consulted %d times for a directly addressable run; want 0", resolved)
	}
	if len(fake.addressed) != 1 || fake.addressed[0] != "run-1" {
		t.Fatalf("addressed %v, want exactly [run-1]", fake.addressed)
	}
}

// A scheduled run is NotFound under its run id, and the intent must still
// reach it. Without this a scheduled engine run can be escalated and never
// resolved -- the exact failure #3883 exists to remove.
func TestHITLDelivererRetriesAgainstTheResolvedWorkflow(t *testing.T) {
	fake := &hitlFakeUpdateClient{known: map[string]HITLAck{"wf-9": {Resumed: true, RequestID: "req-1"}}}
	d := hitlDelivererOver(fake).WithWorkflowIDResolver(func(_ context.Context, runID string) (string, error) {
		if runID != "run-1" {
			t.Fatalf("resolver asked for %q, want run-1", runID)
		}
		return "wf-9", nil
	})

	ack, err := d.Deliver(context.Background(), hitlTestIntent())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !ack.Resumed || ack.RequestID != "req-1" {
		t.Fatalf("ack = %+v, want the resumed ack for req-1", ack)
	}
	want := []string{"run-1", "wf-9"}
	if len(fake.addressed) != 2 || fake.addressed[0] != want[0] || fake.addressed[1] != want[1] {
		t.Fatalf("addressed %v, want %v", fake.addressed, want)
	}
}

// ErrRunNotOpen is the resolver's DEFINITE answer, and only then may the
// operator be told there is no run.
func TestHITLDelivererReportsNotFoundOnlyWhenTheRunIsDefinitelyGone(t *testing.T) {
	fake := &hitlFakeUpdateClient{known: map[string]HITLAck{}}
	d := hitlDelivererOver(fake).WithWorkflowIDResolver(func(context.Context, string) (string, error) {
		return "", ErrRunNotOpen
	})

	if _, err := d.Deliver(context.Background(), hitlTestIntent()); !errors.Is(err, ErrHITLRunNotFound) {
		t.Fatalf("error = %v, want %v", err, ErrHITLRunNotFound)
	}
}

// Every other resolver failure is UNKNOWN. Reporting it as "no such run" tells
// an operator to stop looking for a run that may be alive and holding a slot,
// so it must surface as an unresolved lookup instead.
func TestHITLDelivererDoesNotTurnAnUnknownResolutionIntoNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ambiguous", ErrAmbiguousRunID},
		{"enumeration failed", errors.New("list open workflows: connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &hitlFakeUpdateClient{known: map[string]HITLAck{}}
			d := hitlDelivererOver(fake).WithWorkflowIDResolver(func(context.Context, string) (string, error) {
				return "", tc.err
			})

			_, err := d.Deliver(context.Background(), hitlTestIntent())
			if err == nil {
				t.Fatal("an unresolvable lookup was reported as success")
			}
			if errors.Is(err, ErrHITLRunNotFound) {
				t.Fatalf("error = %v, want it NOT to claim the run is gone", err)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want it to carry %v", err, tc.err)
			}
		})
	}
}

// A resolver that answers with the id that just came back NotFound has nothing
// new to offer, and must not cause a second identical round trip.
func TestHITLDelivererDoesNotReAddressTheSameID(t *testing.T) {
	fake := &hitlFakeUpdateClient{known: map[string]HITLAck{}}
	d := hitlDelivererOver(fake).WithWorkflowIDResolver(func(_ context.Context, runID string) (string, error) {
		return runID, nil
	})

	if _, err := d.Deliver(context.Background(), hitlTestIntent()); !errors.Is(err, ErrHITLRunNotFound) {
		t.Fatalf("error = %v, want %v", err, ErrHITLRunNotFound)
	}
	if len(fake.addressed) != 1 {
		t.Fatalf("addressed %v, want exactly one attempt", fake.addressed)
	}
}

// The workflow's own refusal is the answer. It must reach the caller intact,
// because the daemon maps its CODE onto an HTTP status -- wrapping it as a
// transport failure would turn every considered 409 into a 500.
func TestHITLDelivererReturnsTheWorkflowsRefusalVerbatim(t *testing.T) {
	refusal := hitlRefusal(HITLErrRunSettled, "run %s already settled", "run-1")
	fake := &hitlFakeUpdateClient{
		known:  map[string]HITLAck{"run-1": {}},
		getErr: map[string]error{"run-1": refusal},
	}

	_, err := hitlDelivererOver(fake).Deliver(context.Background(), hitlTestIntent())
	code, message, ok := HITLRefusalCode(err)
	if !ok || code != HITLErrRunSettled {
		t.Fatalf("refusal = (%q, %v) from %v, want %q", code, ok, err, HITLErrRunSettled)
	}
	if message == "" {
		t.Fatal("the refusal reached the caller with no operator-facing message")
	}
}

// The protocol identity is stamped by this side, so a caller cannot deliver an
// unversioned or mislabelled intent even by accident.
func TestHITLDelivererStampsTheProtocolItself(t *testing.T) {
	var seen HITLIntent
	fake := &hitlFakeUpdateClient{known: map[string]HITLAck{"run-1": {RequestID: "req-1"}}}
	d := hitlDelivererOver(&hitlCapturingClient{inner: fake, seen: &seen})

	intent := hitlTestIntent()
	intent.Protocol = "someone-elses.protocol"
	intent.Version = 99
	if _, err := d.Deliver(context.Background(), intent); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if seen.Protocol != HITLProtocol || seen.Version != HITLProtocolVersion {
		t.Fatalf("delivered (%q, %d), want (%q, %d)", seen.Protocol, seen.Version, HITLProtocol, HITLProtocolVersion)
	}
}

// A delivery with no run or no request id is refused HERE, before a round
// trip: an intent with no request id has no idempotency key, so retrying it
// would be indistinguishable from a second decision.
func TestHITLDelivererRefusesUnaddressableIntents(t *testing.T) {
	fake := &hitlFakeUpdateClient{known: map[string]HITLAck{"run-1": {RequestID: "req-1"}}}
	d := hitlDelivererOver(fake)

	noRun := hitlTestIntent()
	noRun.RunID = "  "
	if _, err := d.Deliver(context.Background(), noRun); err == nil {
		t.Fatal("an intent naming no run was delivered")
	}
	noKey := hitlTestIntent()
	noKey.RequestID = ""
	if _, err := d.Deliver(context.Background(), noKey); err == nil {
		t.Fatal("an intent with no request id was delivered")
	}
	if len(fake.addressed) != 0 {
		t.Fatalf("addressed %v, want no round trip at all", fake.addressed)
	}
}

// The request id IS the update id, which is what makes the SERVER deduplicate
// a retried delivery before the workflow ever sees it.
func TestHITLDelivererUsesTheRequestIDAsTheUpdateID(t *testing.T) {
	var seen client.UpdateWorkflowOptions
	fake := &hitlFakeUpdateClient{known: map[string]HITLAck{"run-1": {RequestID: "req-1"}}}
	d := hitlDelivererOver(&hitlCapturingClient{inner: fake, options: &seen})

	if _, err := d.Deliver(context.Background(), hitlTestIntent()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if seen.UpdateID != "req-1" {
		t.Fatalf("UpdateID = %q, want the request id req-1", seen.UpdateID)
	}
	if seen.UpdateName != HITLUpdateName {
		t.Fatalf("UpdateName = %q, want %q", seen.UpdateName, HITLUpdateName)
	}
	if seen.WaitForStage != client.WorkflowUpdateStageCompleted {
		t.Fatalf("WaitForStage = %v, want Completed -- an accepted-but-unfinished update is not an accepted decision", seen.WaitForStage)
	}
}

// hitlCapturingClient records what was submitted and delegates.
type hitlCapturingClient struct {
	inner   *hitlFakeUpdateClient
	seen    *HITLIntent
	options *client.UpdateWorkflowOptions
}

func (c *hitlCapturingClient) UpdateWorkflow(ctx context.Context, options client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error) {
	if c.options != nil {
		*c.options = options
	}
	if c.seen != nil && len(options.Args) == 1 {
		intent, ok := options.Args[0].(HITLIntent)
		if !ok {
			return nil, fmt.Errorf("delivered %T, want HITLIntent", options.Args[0])
		}
		*c.seen = intent
	}
	return c.inner.UpdateWorkflow(ctx, options)
}
