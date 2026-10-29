package startintent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestExactWorkItemJournalRecoveryAcknowledgesOnlyTheAcceptedItem(t *testing.T) {
	for _, ref := range []string{"work-item:42", "work-item:43", "repair"} {
		t.Run(ref, func(t *testing.T) {
			service, starter, _, _ := dispatchFixture(t)
			record, _, err := service.Accept(t.Context(), "exact-recovery", "operator",
				Request{Workflow: "repair", Gaggle: "own", WorkItemID: "42"})
			if err != nil {
				t.Fatal(err)
			}
			runID := strings.TrimPrefix(record.ID, "trigger-")
			root := t.TempDir()
			run, err := journal.Create(root, journal.RunIdentity{
				RunID: runID, Workflow: "repair", Gaggle: "own", WorkflowVersion: 1,
				ConfigGeneration: "generation-1", WorkflowDigest: "workflow-1", GooberDigest: "goober-1",
				Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: ref},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			service.RunDirectory = func(context.Context, string) (string, error) {
				return filepath.Join(root, runID), nil
			}
			matches := ref == "work-item:42"
			err = service.Dispatch(t.Context(), t.Context(), record)
			if (err == nil) != matches {
				t.Fatalf("recovery for %q: %v", ref, err)
			}
			stored, err := service.Queue.Get(t.Context(), record.ID, "operator")
			if err != nil {
				t.Fatal(err)
			}
			want := triggerqueue.Accepted
			if matches {
				want = triggerqueue.Dispatched
			}
			if stored.State != want {
				t.Fatalf("state=%s, want %s", stored.State, want)
			}
			select {
			case <-starter.entered:
				t.Fatal("recovery launched another writer")
			default:
			}
		})
	}
}

func TestExactWorkItemAcceptanceRetainsSelectionAndConflictsOnAnotherItem(t *testing.T) {
	service := intentService(t)
	request := Request{Workflow: "repair", Gaggle: "own", WorkItemID: "42"}
	first, repeated, err := service.Accept(t.Context(), "exact-item-key", "operator", request)
	if err != nil || repeated {
		t.Fatal(first, repeated, err)
	}
	envelope, err := Parse(first.Payload)
	if err != nil || envelope.Request.WorkItemID != "42" {
		t.Fatal(envelope, err)
	}
	retry, repeated, err := service.Accept(t.Context(), "exact-item-key", "operator", request)
	if err != nil || !repeated || retry.ID != first.ID {
		t.Fatal(retry, repeated, err)
	}
	request.WorkItemID = "43"
	if _, _, err := service.Accept(t.Context(), "exact-item-key", "operator", request); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal("a different item reused the original key:", err)
	}
}

func TestExactWorkItemRejectsAmbiguousOrPrivilegedSelectors(t *testing.T) {
	for _, request := range []Request{
		{Workflow: "repair", WorkItemID: "42"},
		{Workflow: "repair", Gaggle: "own", WorkItemID: "0"},
		{Workflow: "repair", Gaggle: "own", WorkItemID: "../42"},
		{Workflow: "repair", Gaggle: "own", WorkItemID: "42", Force: true},
		{Workflow: "repair", Gaggle: "own", WorkItemID: "42", SourceRun: "other-run"},
		{Workflow: "repair", Gaggle: "own", WorkItemID: "42", PodScoped: true, PodRunID: "pod"},
		{Workflow: "repair", Gaggle: "own", WorkItemID: "42", PullRequest: 1},
	} {
		if !errors.Is(request.Validate(), ErrInvalid) {
			t.Fatalf("accepted invalid exact selection: %+v", request)
		}
	}
}
