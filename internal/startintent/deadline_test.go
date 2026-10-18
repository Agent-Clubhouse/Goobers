package startintent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestAdmissionDeadlineRetainsFirstAcceptance(t *testing.T) {
	s := intentService(t)
	deadline := time.Now().Add(time.Minute)
	first, _, err := s.AcceptBefore(t.Context(), "deadline", "alice", Request{Workflow: "repair"}, deadline)
	if err != nil {
		t.Fatal(err)
	}
	s.Capture = func(context.Context, Request) (Target, func(), error) {
		t.Fatal("retry recaptured")
		return Target{}, nil, nil
	}
	retry, duplicate, err := s.AcceptBefore(t.Context(), "deadline", "alice", Request{Workflow: "repair"}, deadline.Add(time.Hour))
	if err != nil || !duplicate || first.ID != retry.ID || string(first.Payload) != string(retry.Payload) {
		t.Fatal(retry, duplicate, err)
	}
	envelope, err := Parse(retry.Payload)
	if err != nil || !envelope.Deadline.Equal(deadline) {
		t.Fatal(envelope, err)
	}
}

func TestExpiredAdmissionReconcilesJournalBeforeRejecting(t *testing.T) {
	for _, mode := range []string{"absent", "published", "mismatch", "unreadable", "live-launch"} {
		t.Run(mode, func(t *testing.T) {
			s, starter, _, _ := dispatchFixture(t)
			record, _, err := s.AcceptBefore(t.Context(), "expired", "alice", Request{Workflow: "repair"}, time.Now().Add(-time.Second))
			if err != nil {
				t.Fatal(err)
			}
			runID := strings.TrimPrefix(record.ID, "trigger-")
			if mode == "published" || mode == "mismatch" {
				identity := journal.RunIdentity{RunID: runID, Workflow: "repair", Gaggle: "own", WorkflowVersion: 1, ConfigGeneration: "generation-1", WorkflowDigest: "workflow-1", GooberDigest: "goober-1", Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "repair"}}
				if mode == "mismatch" {
					identity.ConfigGeneration = "wrong"
				}
				root := t.TempDir()
				run, err := journal.Create(root, identity, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Close(); err != nil {
					t.Fatal(err)
				}
				s.RunDirectory = func(context.Context, string) (string, error) { return filepath.Join(root, runID), nil }
			}
			if mode == "unreadable" {
				s.RunDirectory = func(context.Context, string) (string, error) { return "", errors.New("journal unavailable") }
			}
			if mode == "live-launch" {
				if err := s.Queue.BeginDispatch(t.Context(), record.ID); err != nil {
					t.Fatal(err)
				}
				record, err = s.Queue.Get(t.Context(), record.ID, "alice")
				if err != nil {
					t.Fatal(err)
				}
			}
			err = s.Dispatch(t.Context(), t.Context(), record)
			if (err != nil) != (mode == "mismatch" || mode == "unreadable") {
				t.Fatal(err)
			}
			stored, err := s.Queue.Get(t.Context(), record.ID, "alice")
			want := triggerqueue.Accepted
			switch mode {
			case "absent":
				want = triggerqueue.Rejected
			case "published":
				want = triggerqueue.Dispatched
			case "live-launch":
				want = triggerqueue.Dispatching
			}
			if err != nil || stored.State != want {
				t.Fatal(stored, err)
			}
			select {
			case <-starter.entered:
				t.Fatal("expired request launched")
			default:
			}
		})
	}
}
