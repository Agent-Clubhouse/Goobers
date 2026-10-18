package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func parentArchiveReceiptFixture(t *testing.T, branchIndex int) (*journal.Run, OwnedJournalRecorder, journal.Ref) {
	t.Helper()
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "parent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	rec, err := OwnedBranchRecorder(run, branchIndex)
	if err != nil {
		t.Fatal(err)
	}
	origin := &apiv1.ChildWorkflowOrigin{StageOccurrence: journal.Digest([]byte("stage")), AttemptID: journal.Digest([]byte("attempt"))}
	custody := ContainedParentWorkspaceCustody{Version: 1, Origin: origin, Workspace: worktree.StageCustody{OwnerRunID: "parent", WorkspaceID: "parent-work", RepositoryDigest: journal.Digest([]byte("repo")), Branch: "parent-branch", StartRef: strings.Repeat("a", 40)}}
	output, err := rec.RecordArtifact("output", []byte("verified output"))
	if err != nil {
		t.Fatal(err)
	}
	contribution := parentContribution{Version: 1, ContractDigest: journal.Digest([]byte("contract")), Custody: custody, Output: output}
	for _, event := range []journal.Event{
		{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": ContainedParentWorkspaceKind, "custody": custody}},
		{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": ParentContributionKind, "contractDigest": contribution.ContractDigest, "contribution": contribution}},
	} {
		if err := rec.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	record, err := rec.RecordArtifact("verified-archive-record", []byte("host verified recovery metadata"))
	if err != nil {
		t.Fatal(err)
	}
	return run, rec, record
}

func TestParentArchiveRetirementRestorationCycles(t *testing.T) {
	run, rec, record := parentArchiveReceiptFixture(t, 2)
	if err := RecordParentArchiveRetirement(run, "parent-branch", record); err == nil {
		t.Fatal("root recorder retired a parallel branch")
	}
	var prior uint64
	for range 2 {
		if err := RecordParentArchiveRetirement(rec, "parent-branch", record); err != nil {
			t.Fatal(err)
		}
		state, err := ownedParentArchiveState(rec, "parent-branch")
		if err != nil || state.retiredAt <= prior {
			t.Fatal("retirement cycle missing", state, err)
		}
		if err := RecordParentArchiveRetirement(rec, "parent-branch", record); err != nil {
			t.Fatal("identical retirement retry", err)
		}
		changed := record
		changed.Digest = journal.Digest([]byte("changed"))
		if err := RecordParentArchiveRetirement(rec, "parent-branch", changed); err == nil {
			t.Fatal("changed retirement replay accepted")
		}
		if err := RecordParentArchiveRestoration(rec, state.archive, prior); err == nil {
			t.Fatal("stale restoration unfenced a newer retirement")
		}
		reader, _ := journal.OpenReadOnly(run.Dir())
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		if _, found, err := selectHeldParentContribution(events, "parent", "parent-branch"); err == nil || found {
			t.Fatal("retired workspace remained runnable")
		}
		for range 2 {
			if err := RecordParentArchiveRestoration(rec, state.archive, state.retiredAt); err != nil {
				t.Fatal("restoration replay", err)
			}
		}
		events, err = reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		if _, found, err := selectHeldParentContribution(events, "parent", "parent-branch"); err != nil || !found {
			t.Fatal("restored workspace unavailable", err)
		}
		prior = state.retiredAt
	}
}

func TestParentArchiveResumeRequiresDurableAcknowledgement(t *testing.T) {
	for _, mode := range []string{"missing", "failure", "unacknowledged", "restored"} {
		t.Run(mode, func(t *testing.T) {
			run, rec, record := parentArchiveReceiptFixture(t, 1)
			if err := RecordParentArchiveRetirement(rec, "parent-branch", record); err != nil {
				t.Fatal(err)
			}
			calls := 0
			r := &Runner{}
			if mode != "missing" {
				r.cfg.RestoreParentArchive = func(_ context.Context, owned OwnedJournalRecorder, a ParentWorkspaceArchive, seq uint64) error {
					calls++
					if mode == "failure" {
						return errors.New("archive unavailable")
					}
					if mode == "restored" {
						return RecordParentArchiveRestoration(owned, a, seq)
					}
					return nil
				}
			}
			err := r.restoreParentWorkspaceArchives(t.Context(), run)
			if (err == nil) != (mode == "restored") {
				t.Fatal(mode, err)
			}
			if mode == "restored" {
				if err := r.restoreParentWorkspaceArchives(t.Context(), run); err != nil || calls != 1 {
					t.Fatal("already restored work ran twice", calls, err)
				}
			}
		})
	}
}

func TestParentArchiveWalkCannotCreateExecutorsBeforeRestore(t *testing.T) {
	run, rec, record := parentArchiveReceiptFixture(t, 0)
	if err := RecordParentArchiveRetirement(rec, "parent-branch", record); err != nil {
		t.Fatal(err)
	}
	ws := &walkState{jr: run, in: StartInput{Machine: &workflow.Machine{}}}
	r := &Runner{}
	if _, err := r.walk(t.Context(), ws); err == nil || !strings.Contains(err.Error(), "restoration service unavailable") {
		t.Fatal("walk advanced before restoring retired checkout", err)
	}
	if ws.ex != nil {
		t.Fatal("executors created before restoration")
	}
}
