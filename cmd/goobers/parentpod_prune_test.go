package main

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

func TestTelemetryPrunePreservesUnarchivedParent(t *testing.T) {
	for _, mode := range []string{"writer-pending", "returned", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			layout := writeRecoveryPolicyInstance(t, 0)
			now := time.Now().UTC()
			dir := createTelemetryRetentionRun(t, layout, "parent-held", now.Add(-48*time.Hour))
			run, _, err := journal.Recover(dir)
			if err != nil {
				t.Fatal(err)
			}
			origin := &apiv1.ChildWorkflowOrigin{StageOccurrence: journal.Digest([]byte("stage")), AttemptID: journal.Digest([]byte("attempt"))}
			custody := runner.ContainedParentWorkspaceCustody{Version: 1, Origin: origin, Workspace: worktree.StageCustody{OwnerRunID: "parent-held", WorkspaceID: "workspace", RepositoryDigest: journal.Digest([]byte("repo")), Branch: "parent-branch", StartRef: strings.Repeat("a", 40)}}
			if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": runner.ContainedParentWorkspaceKind, "custody": custody}}); err != nil {
				t.Fatal(err)
			}
			if mode != "writer-pending" {
				output, err := run.RecordArtifact("output", []byte("verified-output"))
				if err != nil {
					t.Fatal(err)
				}
				digest := journal.Digest([]byte("contract"))
				contribution := map[string]any{"version": 1, "contractDigest": digest, "custody": custody, "output": output}
				if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": runner.ParentContributionKind, "contractDigest": digest, "contribution": contribution}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "interrupted" {
				dir = stagePruneJournal(t, dir)
			}
			assertCustodyPreservesJournal(t, layout, dir, now)
		})
	}
}
