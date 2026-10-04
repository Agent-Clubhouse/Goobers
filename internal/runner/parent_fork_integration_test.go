//go:build integration

package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func TestIntegrationParentForkReplayPreservesEditsAndDistinctVisits(t *testing.T) {
	r, run, frame, _, fixture := prepareChildWaitRuntime(t)
	ctx := t.Context()
	r.cfg.ChildWorkflowAdmission = func(*workflow.Machine) error { return nil }
	seed, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{RepoURL: fixture.parent, RunID: "seed-workspace", OwnerRunID: frame.in.RunID, BaseRef: "main", Branch: "runs/seed"})
	if err != nil {
		t.Fatal(err)
	}
	childWorkspaceWrite(t, seed.Path, "main.txt", []byte("seed dirty"))
	childWorkspaceWrite(t, seed.Path, "new.txt", []byte("seed new"))
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	workspace := &stageWorkspace{path: seed.Path, worktree: seed}
	env := apiv1.InvocationEnvelope{Attempt: 1, RunID: frame.in.RunID, ChildWorkflowOrigin: frame.childOrigin}
	if _, err = r.holdContainedParentWorkspace(ctx, frame, workspace, env); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}).CanonicalKey()
	snapshot, err := recovery.CaptureChildSnapshot(ctx, seed.Path, key, frame.in.RunID, at, at.Add(time.Hour), recovery.SnapshotPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	portable, err := recovery.WritePortableSnapshot(ctx, seed.Path, snapshot, &bundle, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	contract := journal.Digest([]byte("seed-contract"))
	data, _ := json.Marshal(recovery.PortableReturn{Version: 1, ContractDigest: contract, Workspace: &recovery.PortableCarrier{Snapshot: portable, Bundle: bundle.Bytes()}})
	ref, err := run.RecordArtifact("seed-output", data)
	if err != nil {
		t.Fatal(err)
	}
	if err = RecordParentContribution(run, env, contract, ref); err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventStageFinished, Stage: frame.t.Name, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: "fan"}); err != nil {
		t.Fatal(err)
	}
	reader, _ := journal.OpenReadOnly(run.Dir())
	fork := func(branch int) *stageWorkspace {
		t.Helper()
		f := frame
		f.t.Name = "branch"
		f.t.RepoFrom = apiv1.RepoFrom{frame.t.Name}
		f.jr = &branchJournal{run: run, branch: branch, setMachineState: func(string) {}}
		if err := r.restoreParentContribution(ctx, &f, branch); err != nil {
			t.Fatal(err)
		}
		return f.heldChildWorkspace
	}
	left, right := fork(1), fork(2)
	if left.path == right.path || left.path == seed.Path {
		t.Fatal("forks share a checkout")
	}
	childWorkspaceWrite(t, left.path, "main.txt", []byte("left progresses"))
	retry := fork(1)
	if retry.path != left.path {
		t.Fatal("fork replay moved checkout")
	}
	childWorkspaceRead(t, retry.path, "main.txt", []byte("left progresses"))
	childWorkspaceRead(t, right.path, "main.txt", []byte("seed dirty"))
	childWorkspaceRead(t, seed.Path, "main.txt", []byte("seed dirty"))
	if err = run.Append(journal.Event{Type: journal.EventParallelFinished, Parallel: "fan"}); err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: "fan"}); err != nil {
		t.Fatal(err)
	}
	later := fork(1)
	if later.path == left.path {
		t.Fatal("later parallel visit inherited prior fork identity")
	}
	childWorkspaceRead(t, later.path, "main.txt", []byte("seed dirty"))
	childWorkspaceRead(t, left.path, "main.txt", []byte("left progresses"))
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := PendingParentContributions(events); !pending {
		t.Fatal("unreturned fork can be pruned")
	}
	// Ready custody must refuse a moved branch; it cannot recreate/reset edits.
	runGit(t, later.path, "checkout", "-b", "tampered")
	f := frame
	f.t.Name = "branch"
	f.t.RepoFrom = apiv1.RepoFrom{frame.t.Name}
	f.jr = &branchJournal{run: run, branch: 1, setMachineState: func(string) {}}
	if err = r.restoreParentContribution(ctx, &f, 1); err == nil {
		t.Fatal("tampered custody recreated")
	}
	if err = os.WriteFile(filepath.Join(run.Dir(), ref.Path), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = r.restoreParentContribution(ctx, &f, 1); err == nil {
		t.Fatal("tampered archived source accepted")
	}
	fixture.assertParentUnchanged(t)
}
