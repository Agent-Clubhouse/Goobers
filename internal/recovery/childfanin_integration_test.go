//go:build integration

package recovery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fanInResult(t *testing.T, repository string, fork ChildSnapshot, name string, edits map[string]string) Record {
	t.Helper()
	child := filepath.Join(t.TempDir(), name)
	recoveryTestGit(t, repository, "worktree", "add", "--detach", child, fork.Record.SnapshotSHA)
	for path, data := range edits {
		childSnapshotWrite(t, child, path, data)
	}
	result, err := CaptureChildTreeResult(t.Context(), child, name, fork, fork.Record.CreatedAt, fork.Record.RetainUntil)
	if err != nil {
		t.Fatal(err)
	}
	if err := PinCommit(t.Context(), repository, result.Record); err != nil {
		t.Fatal(err)
	}
	return result.Record
}

func TestIntegrationChildFanInMergeAndApplication(t *testing.T) {
	repository, key, at := childSnapshotFixture(t)
	policy := SnapshotPolicy{ExcludedPaths: []string{"private.key"}}
	childSnapshotWrite(t, repository, "staging.txt", "staged\n")
	recoveryTestGit(t, repository, "add", "staging.txt")
	childSnapshotWrite(t, repository, "staging.txt", "working\n")
	childSnapshotWrite(t, repository, "private.key", "parent credential\n")
	fork := captureChildFixture(t, repository, key, "fork", at, policy)
	results := []Record{
		fanInResult(t, repository, fork, "a", map[string]string{"a.txt": "a\n", "shared.txt": "same\n", "private.key": "excluded child value\n"}),
		fanInResult(t, repository, fork, "empty", nil),
		fanInResult(t, repository, fork, "b", map[string]string{"b.bin": "\x00\xff", "shared.txt": "same\n"}),
	}
	childSnapshotWrite(t, repository, "parent.txt", "parent work\n")
	parent := captureChildFixture(t, repository, key, "parent-current", at, policy)
	head := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	index := recoveryTestGit(t, repository, "write-tree")
	prepared, err := PrepareChildFanIn(t.Context(), repository, fork, parent, results, "join", at, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	current := captureChildFixture(t, repository, key, "parent-current", at, policy)
	if current.Record.SnapshotSHA != parent.Record.SnapshotSHA || current.IndexDigest != parent.IndexDigest {
		t.Fatal("preparation changed the parent snapshot or index")
	}
	retry, err := PrepareChildFanIn(t.Context(), repository, fork, parent, results, "join", at, 1<<20)
	if err != nil || retry.Prepared.SnapshotSHA != prepared.Prepared.SnapshotSHA {
		t.Fatal("fan-in preparation was not deterministic", err)
	}
	if _, err := PrepareChildFanIn(t.Context(), repository, fork, parent, []Record{results[2], results[1], results[0]}, "join", at, 1<<20); !errors.Is(err, ErrRecordConflict) {
		t.Fatal("changed input order reused the operation pin", err)
	}
	if recoveryTestGit(t, repository, "rev-parse", "HEAD") != head || recoveryTestGit(t, repository, "write-tree") != index {
		t.Fatal("preparation modified live Git state")
	}
	if _, err := os.Stat(filepath.Join(repository, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("preparation wrote branch output", err)
	}
	plan, err := PlanChildApplication(t.Context(), repository, prepared)
	if err != nil {
		t.Fatal(err)
	}
	// The coordinator persists plan before this point. Replaying this exact
	// plan is idempotent; it never reruns a merge against partially applied work.
	for range 2 {
		if err := ApplyChildApplication(t.Context(), repository, plan); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{"a.txt": "a\n", "b.bin": "\x00\xff", "shared.txt": "same\n", "staging.txt": "working\n", "parent.txt": "parent work\n", "private.key": "parent credential\n"} {
		got, err := os.ReadFile(filepath.Join(repository, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", path, got, err)
		}
	}
	if recoveryTestGit(t, repository, "rev-parse", "HEAD") != head || recoveryTestGit(t, repository, "show", ":staging.txt") != "staged" {
		t.Fatal("application changed HEAD or unrelated staging")
	}
	for _, result := range results {
		if recoveryTestGit(t, repository, "rev-parse", result.Ref) != result.SnapshotSHA {
			t.Fatal("fan-in discarded an input")
		}
	}
}

func TestIntegrationChildFanInAggregateBudgetAndPolicy(t *testing.T) {
	repository, key, at := childSnapshotFixture(t)
	fork := captureChildFixture(t, repository, key, "fork", at, SnapshotPolicy{ExcludedPaths: []string{"private.key"}})
	a := fanInResult(t, repository, fork, "a", map[string]string{"a.txt": "a\n"})
	b := fanInResult(t, repository, fork, "b", map[string]string{"b.txt": "b\n"})
	var sum int64
	for _, result := range []Record{a, b} {
		var patch bytes.Buffer
		if _, err := WriteSnapshotPatch(t.Context(), repository, result.BaseSHA, result.SnapshotSHA, &patch); err != nil {
			t.Fatal(err)
		}
		sum += int64(patch.Len())
	}
	refs := recoveryTestGit(t, repository, "show-ref")
	if _, err := PrepareChildFanIn(t.Context(), repository, fork, fork, []Record{a, b}, "budget", at, sum-1); err == nil {
		t.Fatal("per-result limits bypassed the aggregate byte budget")
	}
	if recoveryTestGit(t, repository, "show-ref") != refs {
		t.Fatal("failed preparation published a ref")
	}
	if _, err := PrepareChildFanIn(t.Context(), repository, fork, fork, []Record{a, b}, "exact-budget", at, sum); err != nil {
		t.Fatal("exact aggregate budget refused", err)
	}
	oversized := make([]Record, 129)
	for index := range oversized {
		oversized[index] = a
	}
	if _, err := PrepareChildFanIn(t.Context(), repository, fork, fork, oversized, "too-many", at, 1<<20); err == nil {
		t.Fatal("unbounded result list admitted")
	}
	// A result from a weaker capture policy must not smuggle an excluded path.
	weaker := fork
	weaker.Policy = SnapshotPolicy{}
	unsafe := fanInResult(t, repository, weaker, "unsafe", map[string]string{"private.key": "must not merge\n"})
	if _, err := PrepareChildFanIn(t.Context(), repository, fork, fork, []Record{unsafe}, "policy", at, 1<<20); err == nil {
		t.Fatal("excluded path admitted")
	}
}

func TestIntegrationChildFanInRefusalsPreserveParent(t *testing.T) {
	repository, key, at := childSnapshotFixture(t)
	fork := captureChildFixture(t, repository, key, "fork", at, SnapshotPolicy{})
	a := fanInResult(t, repository, fork, "a", map[string]string{"tracked.txt": "a\n"})
	b := fanInResult(t, repository, fork, "b", map[string]string{"tracked.txt": "b\n"})
	refs := recoveryTestGit(t, repository, "show-ref")
	index := recoveryTestGit(t, repository, "write-tree")
	_, err := PrepareChildFanIn(t.Context(), repository, fork, fork, []Record{a, b}, "conflict", at, 1<<20)
	if !errors.Is(err, ErrIncompatibleSnapshot) || !strings.Contains(err.Error(), "result 2") {
		t.Fatal("conflict did not identify the failing input", err)
	}
	if recoveryTestGit(t, repository, "show-ref") != refs || recoveryTestGit(t, repository, "write-tree") != index {
		t.Fatal("conflict changed refs or index")
	}
	got, _ := os.ReadFile(filepath.Join(repository, "tracked.txt"))
	if string(got) != "base\n" {
		t.Fatal("conflict changed live file")
	}
	for name, mutate := range map[string]func(Record) Record{
		"base":       func(r Record) Record { r.BaseSHA = fork.Record.BaseSHA; return r },
		"digest":     func(r Record) Record { r.PatchDigest = emptyPatchDigest; return r },
		"repository": func(r Record) Record { r.RepositoryKey += "-other"; return r },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PrepareChildFanIn(t.Context(), repository, fork, fork, []Record{mutate(a)}, "wrong-"+name, at, 1<<20); err == nil {
				t.Fatal("substituted input admitted")
			}
		})
	}
	if _, err := PrepareChildFanIn(t.Context(), repository, fork, fork, nil, "empty", at, 1<<20); err == nil {
		t.Fatal("empty input list admitted")
	}
	childSnapshotWrite(t, repository, "tracked.txt", "newer parent\n")
	if _, err := PrepareChildFanIn(t.Context(), repository, fork, fork, []Record{a}, "stale", at.Add(time.Second), 1<<20); !errors.Is(err, ErrWorkspaceChanged) {
		t.Fatal("stale parent admitted", err)
	}
}

func TestIntegrationChildMergeAddAddConflictPreservesParent(t *testing.T) {
	repository, key, at := childSnapshotFixture(t)
	fork := captureChildFixture(t, repository, key, "fork", at, SnapshotPolicy{})
	result := fanInResult(t, repository, fork, "child", map[string]string{"new.txt": "child content\n"})
	childSnapshotWrite(t, repository, "new.txt", "parent staged\n")
	recoveryTestGit(t, repository, "add", "new.txt")
	childSnapshotWrite(t, repository, "new.txt", "parent working\n")
	parent := captureChildFixture(t, repository, key, "parent", at, SnapshotPolicy{})
	_, err := PrepareChildDisposition(t.Context(), repository, fork, parent, result, ChildMerge, "add-add", at, 1<<20)
	if !errors.Is(err, ErrIncompatibleSnapshot) {
		t.Fatal("add/add conflict was not refused", err)
	}
	if err := CheckChildSnapshotCurrent(t.Context(), repository, parent); err != nil {
		t.Fatal("add/add preparation modified parent files or index", err)
	}
}
