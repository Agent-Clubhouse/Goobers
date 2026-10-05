package runner

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestParentContributionReplayAndRetirementCoverage(t *testing.T) {
	_, run, frame := childOriginRuntime(t, &childOriginGoober{})
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	custody := ContainedParentWorkspaceCustody{Version: 1, Origin: frame.childOrigin, Workspace: worktree.StageCustody{WorkspaceID: "workspace", OwnerRunID: frame.in.RunID}}
	if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "work", Attempt: 1, Runner: map[string]any{"kind": ContainedParentWorkspaceKind, "custody": custody}}); err != nil {
		t.Fatal(err)
	}
	ref, err := run.RecordArtifact("returned-tree", []byte("retained tree"))
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{RunID: frame.in.RunID, ChildWorkflowOrigin: frame.childOrigin}
	digest := journal.Digest([]byte("contract"))
	reader, _ := journal.OpenReadOnly(run.Dir())
	before, _ := reader.Events()
	if pending, _ := PendingParentContributions(before); !pending {
		t.Fatal("unreturned held workspace not pinned")
	}
	for range 2 {
		if err := RecordParentContribution(run, env, digest, ref); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := reader.Events()
	if len(events) != len(before)+1 {
		t.Fatal("replay duplicated receipt")
	}
	if pending, err := PendingParentContributions(events); err != nil || !pending {
		t.Fatal(pending, err)
	}
	changed, _ := run.RecordArtifact("other-tree", []byte("different"))
	if err := RecordParentContribution(run, env, digest, changed); err == nil {
		t.Fatal("changed replay accepted")
	}
	last := events[len(events)-1]
	retired := last
	retired.Seq++
	retired.Runner = map[string]any{"kind": ParentContributionRetiredKind, "contractDigest": digest, "contribution": last.Runner["contribution"]}
	events = append(events, retired)
	if pending, err := PendingParentContributions(events); err != nil || pending {
		t.Fatal(pending, err)
	}
	// Another stage/attempt holding the same checkout invalidates old retirement.
	var hold journal.Event
	for _, event := range before {
		if event.Runner["kind"] == ContainedParentWorkspaceKind {
			hold = event
		}
	}
	hold.Seq = retired.Seq + 1
	if pending, _ := PendingParentContributions(append(events, hold)); !pending {
		t.Fatal("new hold inherited old retirement")
	}
	raw, _ := json.Marshal(last.Runner["contribution"])
	var value parentContribution
	_ = json.Unmarshal(raw, &value)
	value.Output = changed
	retired.Runner["contribution"] = value
	if _, err := PendingParentContributions(events); err == nil {
		t.Fatal("retirement switched retained artifact")
	}
}
