package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestParentWorkspaceContinuityFencesIncompleteAndForeignReturns(t *testing.T) {
	for _, mode := range []string{"current", "sibling", "new-hold", "foreign-run", "different-workspace", "retired"} {
		t.Run(mode, func(t *testing.T) {
			origin := &apiv1.ChildWorkflowOrigin{StageOccurrence: journal.Digest([]byte("stage")), AttemptID: journal.Digest([]byte("attempt"))}
			custody := ContainedParentWorkspaceCustody{Version: 1, Origin: origin, Workspace: worktree.StageCustody{OwnerRunID: "parent", WorkspaceID: "parent-work", RepositoryDigest: journal.Digest([]byte("repo")), Branch: "parent-branch", StartRef: "base"}}
			contribution := parentContribution{Version: 1, ContractDigest: journal.Digest([]byte("contract")), Custody: custody, Output: journal.Ref{Path: "artifacts/output.json", Size: 5, Digest: journal.Digest([]byte("bytes"))}}
			if mode == "foreign-run" {
				contribution.Custody.Workspace.OwnerRunID = "another-parent"
			}
			if mode == "different-workspace" {
				contribution.Custody.Workspace.WorkspaceID = "another-checkout"
			}
			events := []journal.Event{
				{Seq: 1, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": ContainedParentWorkspaceKind, "custody": custody}},
				{Seq: 2, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": ParentContributionKind, "contractDigest": contribution.ContractDigest, "contribution": contribution}},
			}
			if mode == "new-hold" {
				events = append(events, journal.Event{Seq: 3, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": ContainedParentWorkspaceKind, "custody": custody}})
			}
			if mode == "retired" {
				events = append(events, journal.Event{Seq: 3, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": ParentContributionRetiredKind, "contractDigest": contribution.ContractDigest, "contribution": contribution}})
			}
			branch := "parent-branch"
			if mode == "sibling" {
				branch = "sibling-branch"
			}
			got, found, err := selectHeldParentContribution(events, "parent", branch)
			switch mode {
			case "current":
				if err != nil || !found || got.Custody.Workspace != custody.Workspace {
					t.Fatal(got, found, err)
				}
			case "sibling":
				if err != nil || found {
					t.Fatal("borrowed sibling contribution", found, err)
				}
			default:
				if err == nil || found {
					t.Fatal("invalid custody was reused", found, err)
				}
			}
		})
	}
}
