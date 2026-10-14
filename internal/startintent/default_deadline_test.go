package startintent

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestDefaultPendingDeadlineStartsAtAcceptanceAndSurvivesRetry(t *testing.T) {
	s := intentService(t)
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	capture := s.Capture
	s.Capture = func(ctx context.Context, request Request) (Target, func(), error) {
		// Capture can take time; the age starts at durable acceptance.
		now = now.Add(time.Minute)
		return capture(ctx, request)
	}
	request := Request{Workflow: "repair"}
	first, _, err := s.Accept(t.Context(), "default-deadline", "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Parse(first.Payload)
	if err != nil || !envelope.Deadline.Equal(first.AcceptedAt.Add(7*24*time.Hour)) {
		t.Fatalf("deadline=%v acceptance=%v err=%v", envelope.Deadline, first.AcceptedAt, err)
	}
	now = now.Add(6 * 24 * time.Hour)
	s.Capture = func(context.Context, Request) (Target, func(), error) {
		t.Fatal("retry recaptured its admission deadline")
		return Target{}, nil, nil
	}
	repeated, duplicate, err := s.Accept(t.Context(), "default-deadline", "alice", request)
	if err != nil || !duplicate || repeated.ID != first.ID || string(repeated.Payload) != string(first.Payload) || !repeated.AcceptedAt.Equal(first.AcceptedAt) {
		t.Fatal(repeated, duplicate, err)
	}
}

func TestDefaultPendingDeadlinePreventsUnlaunchedExecution(t *testing.T) {
	s, starter, _, _ := dispatchFixture(t)
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	record, _, err := s.Accept(t.Context(), "aged-manual", "alice", Request{Workflow: "repair"})
	if err != nil {
		t.Fatal(err)
	}
	now = record.AcceptedAt.Add(7 * 24 * time.Hour)
	if err := s.Dispatch(t.Context(), t.Context(), record); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Queue.Get(t.Context(), record.ID, "alice")
	if err != nil || stored.State != triggerqueue.Rejected || stored.Reason != "accepted trigger expired before execution admission" {
		t.Fatal(stored, err)
	}
	select {
	case <-starter.entered:
		t.Fatal("expired manual start executed")
	default:
	}
}

func TestDefaultPendingDeadlinePreservesExplicitAndExistingDeadlines(t *testing.T) {
	for _, name := range []string{"unbounded", "explicit"} {
		t.Run(name, func(t *testing.T) {
			s := intentService(t)
			deadline := time.Time{}
			if name == "explicit" {
				deadline = time.Now().Add(time.Minute).UTC()
			}
			first, _, err := s.AcceptBefore(t.Context(), name, "alice", Request{Workflow: "repair"}, deadline)
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := Parse(first.Payload)
			if err != nil || !envelope.Deadline.Equal(deadline) {
				t.Fatal(envelope, err)
			}
			// A normal retry of an older or explicitly bounded receipt preserves it.
			repeated, duplicate, err := s.Accept(t.Context(), name, "alice", Request{Workflow: "repair"})
			if err != nil || !duplicate || string(repeated.Payload) != string(first.Payload) {
				t.Fatal(repeated, duplicate, err)
			}
		})
	}
}
