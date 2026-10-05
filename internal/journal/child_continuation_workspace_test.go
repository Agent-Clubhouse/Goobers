package journal

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestChildContinuationWorkspaceRequiresNewOwnedFork(t *testing.T) {
	root := t.TempDir()
	id := childTestIdentity()
	id.WorkspaceBranch = "goobers/children/" + id.RunID
	id.WorkspaceBranchSHA = strings.Repeat("a", 40)
	id.WorkspaceRepository = &apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "owner", Name: "repo"}
	source, err := Create(root, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Append(Event{Type: EventRunFinished, Status: string(PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(filepath.Join(root, id.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotJournalTree(t, reader.Dir())
	lineage := *id.Child
	lineage.ExecutionEpoch, lineage.PriorResultRef, lineage.RestartDigest = 1, Digest([]byte("sealed")), Digest([]byte("plan"))
	request := ContinuationRequest{RunID: strings.Repeat("c", 32), SourceRunID: id.RunID, ExpectedTerminalSeq: events[len(events)-1].Seq, Operator: "issuer:human", Target: "implement", ChildContinuation: &lineage}
	for _, mode := range []string{"missing", "foreign", "changed repository", "invalid sha"} {
		bad := request
		bad.ChildWorkspace = &ChildContinuationWorkspace{Branch: "goobers/children/" + request.RunID, ForkSHA: strings.Repeat("b", 40)}
		switch mode {
		case "missing":
			bad.ChildWorkspace = nil
		case "foreign":
			bad.ChildWorkspace.Branch = id.WorkspaceBranch
		case "changed repository":
			other := *id.WorkspaceRepository
			other.Name = "other"
			bad.SourceRepository = &other
		case "invalid sha":
			bad.ChildWorkspace.ForkSHA = "mutable"
		}
		if created, err := CreateContinuation(root, bad); err == nil {
			_ = created.Close()
			t.Fatal(mode, "accepted")
		}
	}
	request.ChildWorkspace = &ChildContinuationWorkspace{Branch: "goobers/children/" + request.RunID, ForkSHA: strings.Repeat("b", 40)}
	next, err := CreateContinuation(root, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
	nextReader, err := OpenReadOnly(next.Dir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := nextReader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceBranch != request.ChildWorkspace.Branch || got.WorkspaceBranchSHA != request.ChildWorkspace.ForkSHA || !reflect.DeepEqual(got.WorkspaceRepository, id.WorkspaceRepository) {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(before, snapshotJournalTree(t, reader.Dir())) {
		t.Fatal("source changed")
	}
}
