//go:build integration

package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationReservedStageHoldPreservesDirtyStateAcrossRestart(t *testing.T) {
	testdep.Require(t, "git")
	for _, state := range []string{"active", "held", "interrupted-hold", "foreign-owner", "foreign-start", "replacement-owner", "replacement-start", "cleanup", "partial-restore", "changed-branch"} {
		t.Run(state, func(t *testing.T) { verifyReservedStageHold(t, state) })
	}
}

func verifyReservedStageHold(t *testing.T, state string) {
	t.Helper()
	manager, repo := pinnedFixture(t)
	wt, err := manager.Create(t.Context(), CreateOptions{RepoURL: repo, RunID: "join-workspace", OwnerRunID: "parent", Gaggle: "web", BaseRef: "main", Branch: "goobers/parent"})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(wt.Path, "committed.txt"), "committed\n")
	runTestGit(t, wt.Path, "add", "committed.txt")
	runTestGit(t, wt.Path, "commit", "-m", "progress")
	mustWriteFile(t, filepath.Join(wt.Path, "staged.txt"), "staged\n")
	runTestGit(t, wt.Path, "add", "staged.txt")
	mustWriteFile(t, filepath.Join(wt.Path, "staged.txt"), "working\n")
	mustWriteFile(t, filepath.Join(wt.Path, "untracked.bin"), "\x00\xff")
	custody, err := wt.StageIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AdoptHeldStage(t.Context(), repo, custody); err == nil {
		t.Fatal("reading identity granted held custody")
	}
	primary, ownership, err := wt.custodyMarkers()
	if err != nil {
		t.Fatal(err)
	}
	wantFailure := mutateReservedStageFixture(t, wt, state, &primary, &ownership)
	if err := wt.writeCustodyMarkers(primary, ownership); err != nil {
		t.Fatal(err)
	}
	head, index := runTestGit(t, wt.Path, "rev-parse", "HEAD"), runTestGit(t, wt.Path, "write-tree")
	manager, err = NewManager(manager.Root, WithRemoteGitGate(func(context.Context, string) error { return errors.New("unexpected remote acquisition") }))
	if err != nil {
		t.Fatal(err)
	}
	held, err := manager.HoldReservedStage(t.Context(), repo, custody)
	if (err != nil) != wantFailure {
		t.Fatal("reservation recovery result", state, err)
	}
	if runTestGit(t, wt.Path, "rev-parse", "HEAD") != head || runTestGit(t, wt.Path, "write-tree") != index {
		t.Fatal("hold changed HEAD or index")
	}
	for name, want := range map[string]string{"staged.txt": "working\n", "untracked.bin": "\x00\xff"} {
		data, err := os.ReadFile(filepath.Join(wt.Path, name))
		if err != nil || string(data) != want {
			t.Fatal("hold changed work", name, err)
		}
	}
	if wantFailure {
		for path, before := range map[string]marker{manager.markerPath(wt.key, wt.RunID): primary, manager.ownershipPath(wt.key, primary.Directory): ownership} {
			after, err := readMarker(path)
			if err != nil || after != before {
				t.Fatal("refused hold changed ownership", after, err)
			}
		}
		return
	}
	if held.Path != wt.Path {
		t.Fatal("hold created a substitute checkout")
	}
	if _, err := manager.AdoptHeldStage(t.Context(), repo, custody); err != nil {
		t.Fatal("hold did not settle both markers", err)
	}
	if retried, err := held.HoldForChild(t.Context()); err != nil || retried != custody {
		t.Fatal("ordinary hold retry changed identity", retried, err)
	}
}

func mutateReservedStageFixture(t *testing.T, wt *Worktree, state string, primary, ownership *marker) bool {
	t.Helper()
	switch state {
	case "held", "interrupted-hold":
		ownership.Status, ownership.CleanupDisposition, ownership.StartRef = statusCleanupRetained, childWaitDisposition, primary.StartRef
		if state == "held" {
			primary.Status, primary.CleanupDisposition = statusCleanupRetained, childWaitDisposition
		}
	case "foreign-owner":
		ownership.OwnerRunID = "other-parent"
	case "foreign-start":
		ownership.StartRef = "changed"
	case "replacement-owner":
		primary.OwnerRunID, ownership.OwnerRunID = "replacement", "replacement"
	case "replacement-start":
		head := runTestGit(t, wt.Path, "rev-parse", "HEAD")
		primary.StartRef, ownership.StartRef = strings.TrimSpace(head), strings.TrimSpace(head)
	case "cleanup":
		primary.Status, ownership.Status = statusCleanupPending, statusCleanupPending
	case "partial-restore":
		primary.ParentRestoreHead, ownership.ParentRestoreHead = primary.StartRef, primary.StartRef
	case "changed-branch":
		runTestGit(t, wt.Path, "checkout", "-b", "foreign")
	}
	return state != "active" && state != "held" && state != "interrupted-hold"
}
