//go:build integration

package recovery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

type parentStateFixture struct {
	git     ParentGitState
	working PortableSnapshot
	bundles [3][]byte
}

func captureParentStateFixture(t *testing.T, repo string, snapshot ChildSnapshot) parentStateFixture {
	t.Helper()
	var result parentStateFixture
	for i, state := range []*PortableSnapshot{&result.git.Head, &result.git.Index, &result.working} {
		var data bytes.Buffer
		var err error
		if i == 2 {
			*state, err = WritePortableSnapshot(t.Context(), repo, snapshot, &data, 16<<20)
		} else {
			*state, err = WritePortableGitState(t.Context(), repo, snapshot, i == 1, &data, 16<<20)
		}
		if err != nil {
			t.Fatal(err)
		}
		result.bundles[i] = data.Bytes()
	}
	return result
}

func importParentStateFixture(t *testing.T, repo string, state parentStateFixture, initialize bool) {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "state.bundle")
	for i, portable := range []PortableSnapshot{state.git.Head, state.git.Index, state.working} {
		if err := os.WriteFile(archive, state.bundles[i], 0600); err != nil {
			t.Fatal(err)
		}
		var err error
		if initialize && i == 0 {
			err = InitializePortableWorkspace(t.Context(), repo, archive, portable, 16<<20)
		} else {
			err = ImportPortableSnapshot(t.Context(), repo, archive, portable, 16<<20)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if initialize {
		if err := MaterializePortableGitState(t.Context(), repo, state.git.Head, state.git.Index, state.working); err != nil {
			t.Fatal(err)
		}
	}
}

func parentApplicationFixture(t *testing.T) (string, ChildApplyPlan) {
	t.Helper()
	repo, key, at := childSnapshotFixture(t)
	childSnapshotWrite(t, repo, "credential", "committed secret\n")
	recoveryTestGit(t, repo, "add", "credential")
	recoveryTestGit(t, repo, "commit", "-m", "protected original")
	childSnapshotWrite(t, repo, "credential", "staged secret\n")
	childSnapshotWrite(t, repo, "tracked.txt", "input staged\n")
	recoveryTestGit(t, repo, "add", "credential", "tracked.txt")
	childSnapshotWrite(t, repo, "credential", "working secret\n")
	childSnapshotWrite(t, repo, "tracked.txt", "input dirty\n")
	policy := SnapshotPolicy{ExcludedPaths: []string{"credential"}}
	expected := captureChildFixture(t, repo, key, "parent", at, policy)
	input := captureParentStateFixture(t, repo, expected)
	pod := t.TempDir()
	importParentStateFixture(t, pod, input, true)
	recoveryTestGit(t, pod, "commit", "-m", "worker commit of initial index")
	childSnapshotWrite(t, pod, "tracked.txt", "output staged\n")
	recoveryTestGit(t, pod, "add", "tracked.txt")
	childSnapshotWrite(t, pod, "tracked.txt", "output dirty\n")
	childSnapshotWrite(t, pod, "untracked.bin", "\x00\xff")
	output := captureParentStateFixture(t, pod, captureChildFixture(t, pod, key, "parent", at, policy))
	importParentStateFixture(t, repo, output, false)
	plan, err := PrepareParentApplication(t.Context(), repo, expected, input.git, output.git, output.working, "parent-apply", at, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckChildSnapshotCurrent(t.Context(), repo, expected); err != nil {
		t.Fatal("planning mutated host state", err)
	}
	return repo, plan
}

func TestIntegrationParentApplicationRecoversEachMutationBoundary(t *testing.T) {
	testdep.Require(t, "git")
	for _, boundary := range []string{"before", "files", "index", "head"} {
		t.Run(boundary, func(t *testing.T) {
			repo, plan := parentApplicationFixture(t)
			if boundary != "before" {
				simulateParentApplicationBoundary(t, repo, plan, boundary)
			}
			for range 2 {
				if err := ApplyChildApplication(t.Context(), repo, plan); err != nil {
					t.Fatal("replay failed", err)
				}
			}
			if recoveryTestGit(t, repo, "rev-parse", "HEAD^") != plan.Disposition.ExpectedParent.Record.BaseSHA {
				t.Fatal("replay duplicated checkpoint or published synthetic ancestry")
			}
			for ref, want := range map[string]string{"HEAD:tracked.txt": "input staged", ":tracked.txt": "output staged", "HEAD:credential": "committed secret", ":credential": "staged secret"} {
				if got := recoveryTestGit(t, repo, "show", ref); got != want {
					t.Fatalf("%s = %q, want %q", ref, got, want)
				}
			}
			for name, want := range map[string]string{"tracked.txt": "output dirty\n", "credential": "working secret\n", "untracked.bin": "\x00\xff"} {
				data, err := os.ReadFile(filepath.Join(repo, name))
				if err != nil || string(data) != want {
					t.Fatalf("working %s = %q: %v", name, data, err)
				}
			}
			if got := recoveryTestGit(t, repo, "ls-files", "untracked.bin"); got != "" {
				t.Fatal("return staged an untracked worker file")
			}
		})
	}
}

func simulateParentApplicationBoundary(t *testing.T, repo string, plan ChildApplyPlan, boundary string) {
	t.Helper()
	changes, err := loadChildChanges(t.Context(), repo, plan.Disposition)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, change := range changes {
		if err := writeChildFile(root, change, plan.Disposition.Prepared.SnapshotSHA); err != nil {
			t.Fatal(err)
		}
	}
	if boundary == "files" {
		return
	}
	if err := applyApplicationIndex(t.Context(), repo, plan, changes); err != nil {
		t.Fatal(err)
	}
	if boundary == "head" {
		recoveryTestGit(t, repo, "update-ref", "HEAD", plan.Parent.HeadSHA, plan.Disposition.ExpectedParent.Record.BaseSHA)
	}
}

func TestIntegrationParentApplicationRefusesInterveningState(t *testing.T) {
	testdep.Require(t, "git")
	for _, changed := range []string{"head", "index", "files", "intent"} {
		t.Run(changed, func(t *testing.T) {
			repo, plan := parentApplicationFixture(t)
			switch changed {
			case "head":
				recoveryTestGit(t, repo, "commit", "--allow-empty", "-m", "intervening owner")
			case "index":
				recoveryTestGit(t, repo, "add", "tracked.txt")
			case "files":
				childSnapshotWrite(t, repo, "tracked.txt", "intervening edit\n")
			case "intent":
				plan.Parent.HeadSHA = plan.Parent.After.Head.Record.SnapshotSHA
			}
			before := captureChildFixture(t, repo, plan.Disposition.ExpectedParent.Record.RepositoryKey, "check", plan.Disposition.ExpectedParent.Record.CreatedAt, plan.Disposition.ExpectedParent.Policy)
			err := ApplyChildApplication(t.Context(), repo, plan)
			if err == nil || (changed != "intent" && !errors.Is(err, ErrWorkspaceChanged)) {
				t.Fatalf("intervening %s accepted: %v", changed, err)
			}
			if err := CheckChildSnapshotCurrent(t.Context(), repo, before); err != nil {
				t.Fatal("refused application modified host", err)
			}
		})
	}
}
