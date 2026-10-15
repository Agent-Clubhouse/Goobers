package main

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

func TestParentContributionPinsHeldCheckoutAndRefusesReplacement(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-branch", "wrong-owner", "duplicate-hold", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			f := containedParentFixture(t)
			run, env := configuredChildStage(t, f)
			seq, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1, Branch: 2}, false)
			if err != nil || seq == 0 {
				t.Fatal(err)
			}
			env.ChildWorkflowOrigin = origin
			rec, err := runner.OwnedBranchRecorder(run, 2)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runner.ParentWorkspaceCustody(rec, env); err == nil {
				t.Fatal("missing checkout hold admitted")
			}
			custody := runner.ContainedParentWorkspaceCustody{Version: 1, Origin: origin, Workspace: worktree.StageCustody{WorkspaceID: env.RunID + "-branch2", OwnerRunID: env.RunID, RepositoryDigest: worktree.RepositoryDigest("https://example.com/repo.git"), Branch: "goobers/parent", StartRef: "abc123"}}
			if mode == "wrong-owner" {
				custody.Workspace.OwnerRunID = "other-run"
			}
			marker := journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": runner.ContainedParentWorkspaceKind, "custody": custody}}
			if err := rec.Append(marker); err != nil {
				t.Fatal(err)
			}
			if mode == "wrong-branch" {
				rec = run
			}
			if mode == "duplicate-hold" {
				if err := rec.Append(marker); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "replacement" {
				if _, _, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 2, Branch: 2}, true); err != nil {
					t.Fatal(err)
				}
			}
			contract := journal.Digest([]byte("physical contract"))
			raw, err := json.Marshal(map[string]any{"version": 1, "contractDigest": contract})
			if err != nil {
				t.Fatal(err)
			}
			output, err := rec.RecordArtifact("parent-output.json", raw)
			if err != nil {
				t.Fatal(err)
			}
			before := run.Seq()
			err = runner.RecordParentContribution(rec, env, contract, output)
			if mode != "valid" {
				if err == nil || run.Seq() != before {
					t.Fatal("invalid contribution changed custody", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			before = run.Seq()
			if err := runner.RecordParentContribution(rec, env, contract, output); err != nil || run.Seq() != before {
				t.Fatal("replay duplicated contribution", err)
			}
			changed, err := rec.RecordArtifact("changed-output.json", []byte("changed output"))
			if err != nil {
				t.Fatal(err)
			}
			before = run.Seq()
			if err := runner.RecordParentContribution(rec, env, contract, changed); err == nil || run.Seq() != before {
				t.Fatal("replay replaced imported tree", err)
			}
		})
	}
}
