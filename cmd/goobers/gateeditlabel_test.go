package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// TestLabelGateEdit covers TUT-A3's review-routing half (#1215): a removal or
// loosening gets the stricter label (and never the tuning one), a tuning
// change gets the lighter label, and a reclassifying repass swaps the label
// rather than accumulating both.
func TestLabelGateEdit(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	t.Run("removed -> gate-removal label, no tuning label", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(20, "tutor pr")
		provider := server.newGitHubProvider("token")
		if err := labelGateEdit(context.Background(), provider, repo, 20, "removed", "local-ci-gate"); err != nil {
			t.Fatalf("labelGateEdit: %v", err)
		}
		if !issueHasLabel(server, 20, tutorGateRemovalLabel) {
			t.Fatal("expected tutor:gate-removal to be applied")
		}
		if issueHasLabel(server, 20, tutorGateTuningLabel) {
			t.Fatal("tutor:gate-tuning must not be applied for a removal")
		}
	})

	t.Run("loosened -> gate-removal label", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(21, "tutor pr")
		provider := server.newGitHubProvider("token")
		if err := labelGateEdit(context.Background(), provider, repo, 21, "loosened", "local-ci-gate"); err != nil {
			t.Fatalf("labelGateEdit: %v", err)
		}
		if !issueHasLabel(server, 21, tutorGateRemovalLabel) {
			t.Fatal("expected tutor:gate-removal to be applied for a loosening")
		}
	})

	t.Run("tuning -> gate-tuning label, no removal label", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(22, "tutor pr")
		provider := server.newGitHubProvider("token")
		if err := labelGateEdit(context.Background(), provider, repo, 22, "tuning", "local-ci-gate"); err != nil {
			t.Fatalf("labelGateEdit: %v", err)
		}
		if !issueHasLabel(server, 22, tutorGateTuningLabel) {
			t.Fatal("expected tutor:gate-tuning to be applied")
		}
		if issueHasLabel(server, 22, tutorGateRemovalLabel) {
			t.Fatal("tutor:gate-removal must not be applied for tuning")
		}
	})

	t.Run("reclassified repass swaps the label instead of accumulating", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(23, "tutor pr")
		provider := server.newGitHubProvider("token")
		if err := labelGateEdit(context.Background(), provider, repo, 23, "removed", "local-ci-gate"); err != nil {
			t.Fatalf("labelGateEdit (removed): %v", err)
		}
		if err := labelGateEdit(context.Background(), provider, repo, 23, "tuning", "local-ci-gate"); err != nil {
			t.Fatalf("labelGateEdit (tuning): %v", err)
		}
		if issueHasLabel(server, 23, tutorGateRemovalLabel) {
			t.Fatal("tutor:gate-removal should have been cleared on reclassification to tuning")
		}
		if !issueHasLabel(server, 23, tutorGateTuningLabel) {
			t.Fatal("expected tutor:gate-tuning after reclassification")
		}
	})

	t.Run("none -> no labels applied", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(24, "tutor pr")
		provider := server.newGitHubProvider("token")
		if err := labelGateEdit(context.Background(), provider, repo, 24, "none", ""); err != nil {
			t.Fatalf("labelGateEdit: %v", err)
		}
		if issueHasLabel(server, 24, tutorGateRemovalLabel) || issueHasLabel(server, 24, tutorGateTuningLabel) {
			t.Fatal("no label should be applied for classification \"none\"")
		}
	})
}

// TestGateEditClassificationFromJournal covers the read-back half of the
// hand-off between gate-removal-guard and open-pr: a later stage recovers the
// guard's classification straight from the journal's stage.finished
// Outputs, the same scalar-merge pattern claimedIssueFromJournal uses.
func TestGateEditClassificationFromJournal(t *testing.T) {
	root := initDemo(t)
	const runID = "run-1"

	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "tutor", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "gate-removal-guard", Attempt: 1, Status: "success",
		Outputs: map[string]any{"gateEdit": "removed", "subject": "local-ci-gate"},
	}); err != nil {
		t.Fatalf("record gate-removal-guard stage: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	kind, subject, err := gateEditClassificationFromJournal(root, runID)
	if err != nil {
		t.Fatalf("gateEditClassificationFromJournal: %v", err)
	}
	if kind != "removed" || subject != "local-ci-gate" {
		t.Fatalf("kind, subject = %q, %q; want removed, local-ci-gate", kind, subject)
	}
}

func TestGateEditClassificationFromJournalMissingRunIsEmpty(t *testing.T) {
	root := initDemo(t)
	kind, subject, err := gateEditClassificationFromJournal(root, "no-such-run")
	if err != nil {
		t.Fatalf("a run with no journal on this host is not an error, got: %v", err)
	}
	if kind != "" || subject != "" {
		t.Fatalf("kind, subject = %q, %q; want empty for a nonexistent run", kind, subject)
	}
}
