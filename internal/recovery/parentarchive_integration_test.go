//go:build integration

package recovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParentRetentionRefusesUnsupportedRestoreBeforeCleanup(t *testing.T) {
	testdep.Require(t, "git")
	for _, scenario := range []string{"working-path-limit", "index-path-limit", "working-byte-limit", "file-directory-replacement"} {
		t.Run(scenario, func(t *testing.T) {
			repo, key, at := childSnapshotFixture(t)
			base := recoveryTestGit(t, repo, "rev-parse", "HEAD")
			switch scenario {
			case "working-path-limit", "index-path-limit":
				for i := range maxChildApplyPaths + 1 {
					childSnapshotWrite(t, repo, fmt.Sprintf("added/%04d", i), "new work\n")
				}
				if scenario == "index-path-limit" {
					recoveryTestGit(t, repo, "add", "added")
					if err := os.RemoveAll(filepath.Join(repo, "added")); err != nil {
						t.Fatal(err)
					}
				}
			case "working-byte-limit":
				// Highly compressible content fits the bundle budget but exceeds
				// the restored file-content budget. Archive size is insufficient.
				childSnapshotWrite(t, repo, "large.txt", strings.Repeat("x", maxChildApplyBytes+1))
			case "file-directory-replacement":
				if err := os.Remove(filepath.Join(repo, "tracked.txt")); err != nil {
					t.Fatal(err)
				}
				childSnapshotWrite(t, repo, "tracked.txt/child.txt", "new directory\n")
			}
			policy := SnapshotPolicy{}
			before := captureChildFixture(t, repo, key, "parent", at, policy)
			root := t.TempDir()
			request := RetentionRequest{Repository: repo, RepositoryKey: key, RunID: "parent", BaseRef: base, IdentityTime: at, RetainUntil: at.Add(time.Hour), InventoryRoot: root, CleanupRoots: []string{repo}, MaxSnapshots: 2, MaxArchiveBytes: 1 << 20, ParentPolicy: &policy}
			acknowledged := false
			log := retentionJournalFunc(func(journal.Event) error { acknowledged = true; return nil })
			record, path, err := Retain(t.Context(), request, log)
			if err == nil || record != (Record{}) || path != "" || acknowledged {
				t.Fatalf("unsupported restore acknowledged cleanup: %+v %q %v, acknowledged=%v", record, path, err, acknowledged)
			}
			if !strings.Contains(err.Error(), "parent archive cannot be restored automatically") {
				t.Fatal("retention failed before checking restoration limits", err)
			}
			if err := CheckChildSnapshotCurrent(t.Context(), repo, before); err != nil {
				t.Fatal("failed retention changed source", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatal("unsupported restore consumed inventory capacity", entries, err)
			}
		})
	}
}

func TestIntegrationParentRetentionKeepsLatestCommitIndexAndWorkingState(t *testing.T) {
	testdep.Require(t, "git")
	repo, key, at := childSnapshotFixture(t)
	childSnapshotWrite(t, repo, "private/committed", "original repository value\n")
	recoveryTestGit(t, repo, "add", "private/committed")
	recoveryTestGit(t, repo, "commit", "-m", "original excluded source path")
	base := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	childSnapshotWrite(t, repo, "tracked.txt", "ordinary stage commit\n")
	recoveryTestGit(t, repo, "add", "tracked.txt")
	recoveryTestGit(t, repo, "commit", "-m", "ordinary stage after parent return")
	head := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	childSnapshotWrite(t, repo, "tracked.txt", "latest staging\n")
	childSnapshotWrite(t, repo, "private/token", "runtime secret\n")
	childSnapshotWrite(t, repo, "private/committed", "injected runtime replacement\n")
	recoveryTestGit(t, repo, "add", "tracked.txt", "private")
	childSnapshotWrite(t, repo, "tracked.txt", "latest working files\n")
	childSnapshotWrite(t, repo, "untracked.bin", "\x00\xff")
	policy := SnapshotPolicy{ExcludedPaths: []string{"private"}}
	before := captureChildFixture(t, repo, key, "parent", at, policy)
	request := RetentionRequest{Repository: repo, RepositoryKey: key, RunID: "parent", BaseRef: base, IdentityTime: at, RetainUntil: at.Add(24 * time.Hour), InventoryRoot: t.TempDir(), CleanupRoots: []string{repo}, MaxSnapshots: 2, MaxArchiveBytes: 1 << 20, ParentPolicy: &policy}
	blocked := errors.New("journal acknowledgement unavailable")
	log := retentionJournalFunc(func(journal.Event) error { return blocked })
	if record, path, err := Retain(t.Context(), request, log); !errors.Is(err, blocked) || record != (Record{}) || path != "" {
		t.Fatalf("unacknowledged parent archive permits cleanup: %+v %s %v", record, path, err)
	}
	blocked = nil
	record, recordPath, err := Retain(t.Context(), request, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckChildSnapshotCurrent(t.Context(), repo, before); err != nil {
		t.Fatal("archive capture changed source", err)
	}
	again, againPath, err := Retain(t.Context(), request, log)
	if err != nil || again != record || againPath != recordPath {
		t.Fatal("archive replay changed identity", err)
	}
	// Import into a fresh repository holding only the original base. Later
	// ordinary commits, the index root, and working files must come from the
	// retained bundle, not from a surviving source worktree or worker output.
	restored := t.TempDir()
	recoveryTestGit(t, restored, "init")
	recoveryTestGit(t, restored, "fetch", repo, base)
	archive := filepath.Join(filepath.Dir(recordPath), BundleFileName)
	if err := ImportSnapshotBundle(t.Context(), restored, archive, record, request.MaxArchiveBytes); err != nil {
		t.Fatal(err)
	}
	state, err := ReadRetainedParentState(t.Context(), restored, record)
	if err != nil || state.HeadSHA != head {
		t.Fatal("archive lost original commit identity", state, err)
	}
	for ref, want := range map[string]string{state.HeadSHA + ":tracked.txt": "ordinary stage commit", state.IndexSHA + ":tracked.txt": "latest staging", record.SnapshotSHA + ":tracked.txt": "latest working files", record.SnapshotSHA + ":untracked.bin": "\x00\xff"} {
		if got := recoveryTestGit(t, restored, "show", ref); got != want {
			t.Fatalf("%s = %q, want %q", ref, got, want)
		}
	}
	for _, tree := range []string{state.IndexSHA, record.SnapshotSHA} {
		if got := recoveryTestGit(t, restored, "ls-tree", "-r", "--name-only", tree, "--", "private/token"); got != "" {
			t.Fatal("archive included excluded runtime state", got)
		}
	}
	if got := recoveryTestGit(t, restored, "show", record.SnapshotSHA+":private/committed"); got != "original repository value" {
		t.Fatal("exclusion became a repository deletion or injected replacement", got)
	}
	if got := recoveryTestGit(t, restored, "ls-tree", "-r", "--name-only", state.IndexSHA, "--", "private"); got != "" {
		t.Fatal("archive retained excluded staged injection", got)
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRetainedParentState(t.Context(), restored, record); err != nil {
		t.Fatal("archive depended on removed checkout", err)
	}
	verifyParentArchiveRestoration(t, restored, record, state)
}

func verifyParentArchiveRestoration(t *testing.T, restored string, record Record, state RetainedParentState) {
	t.Helper()
	recoveryTestGit(t, restored, "checkout", "-b", "restored-parent", record.BaseSHA)
	if _, err := PlanRetainedParentRestore(t.Context(), restored, record, "restore-parent", 1<<20); err == nil {
		t.Fatal("restore accepted a checkout at the wrong HEAD")
	}
	recoveryTestGit(t, restored, "reset", "--hard", state.HeadSHA)
	childSnapshotWrite(t, restored, "intervening.txt", "new owner work")
	if _, err := PlanRetainedParentRestore(t.Context(), restored, record, "restore-parent", 1<<20); err == nil {
		t.Fatal("restore accepted an already modified checkout")
	}
	if err := os.Remove(filepath.Join(restored, "intervening.txt")); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanRetainedParentRestore(t.Context(), restored, record, "restore-parent", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Replay uses serialized host intent, never fresh planning against partially
	// restored files. HEAD must remain the archived ordinary-stage commit.
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var durable ChildApplyPlan
	if err := json.Unmarshal(data, &durable); err != nil {
		t.Fatal(err)
	}
	simulateParentApplicationBoundary(t, restored, durable, "files")
	for range 2 {
		if err := ApplyChildApplication(t.Context(), restored, durable); err != nil {
			t.Fatal("resume archive restoration", err)
		}
	}
	if got := recoveryTestGit(t, restored, "rev-parse", "HEAD"); got != state.HeadSHA {
		t.Fatal("restore replaced real ancestry", got)
	}
	if got := recoveryTestGit(t, restored, "show", ":tracked.txt"); got != "latest staging" {
		t.Fatal("restore flattened staging", got)
	}
	for name, want := range map[string]string{"tracked.txt": "latest working files\n", "private/committed": "original repository value\n", "untracked.bin": "\x00\xff"} {
		data, err := os.ReadFile(filepath.Join(restored, name))
		if err != nil || string(data) != want {
			t.Fatalf("restored %s = %q: %v", name, data, err)
		}
	}
	if got := recoveryTestGit(t, restored, "ls-files", "untracked.bin", "private/token"); got != "" {
		t.Fatal("restore staged untracked or excluded runtime files", got)
	}
}

func TestIntegrationParentRetentionKeepsStagingOnlyChanges(t *testing.T) {
	testdep.Require(t, "git")
	repo, key, at := childSnapshotFixture(t)
	childSnapshotWrite(t, repo, "tracked.txt", "staged only\n")
	recoveryTestGit(t, repo, "add", "tracked.txt")
	childSnapshotWrite(t, repo, "tracked.txt", "base\n")
	policy := SnapshotPolicy{}
	request := RetentionRequest{Repository: repo, RepositoryKey: key, RunID: "parent", BaseRef: "main", IdentityTime: at, RetainUntil: at.Add(time.Hour), InventoryRoot: t.TempDir(), CleanupRoots: []string{repo}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20, ParentPolicy: &policy, SkipEmpty: true}
	record, _, err := Retain(t.Context(), request, retentionJournalFunc(func(journal.Event) error { return nil }))
	if err != nil || record.SnapshotSHA == "" || record.PatchDigest != emptyPatchDigest {
		t.Fatal("staging-only state was discarded", record, err)
	}
	state, err := ReadRetainedParentState(t.Context(), repo, record)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryTestGit(t, repo, "show", state.IndexSHA+":tracked.txt"); got != "staged only" {
		t.Fatal("index-only state missing", got)
	}
}
