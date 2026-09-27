//go:build integration

package recovery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationCaptureSnapshotPreservesWorktreeAndIndex(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	for name, content := range map[string]string{"tracked.txt": "base", "deleted.txt": "remove", ".gitignore": "ignored.out\nstaged.out\n"} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "base")
	head := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(repository, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"tracked.txt": "changed\r\n", "new.bin": "\x00\xff\x01", "ignored.out": "generated", "local-only.out": "private", "custom-excluded.out": "private", "[literal].txt": "literal", "staged.out": "intentionally tracked"} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recoveryTestGit(t, repository, "add", "--force", "staged.out")
	indexPath := filepath.Join(repository, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	// A clean driver can replace content with a pointer (as LFS does). A
	// self-contained archive must retain the actual worktree bytes instead.
	recoveryTestGit(t, repository, "config", "filter.recovery-test.clean", "git hash-object --stdin")
	recoveryTestGit(t, repository, "config", "filter.recovery-test.required", "true")
	attributes := []byte("* filter=recovery-test text eol=lf working-tree-encoding=UTF-16LE\n")
	if err := os.WriteFile(filepath.Join(repository, ".git", "info", "attributes"), attributes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".git", "info", "exclude"), []byte("local-only.out\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	excludes := filepath.Join(t.TempDir(), "excludes")
	if err := os.WriteFile(excludes, []byte("custom-excluded.out\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "config", "core.excludesFile", excludes)
	subdirectory := filepath.Join(repository, "nested")
	if err := os.Mkdir(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	pathList, err := snapshotPaths(context.Background(), repository, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	selected, err := os.ReadFile(pathList)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Split(strings.TrimSuffix(string(selected), "\x00"), "\x00"), []string{"[literal].txt", "new.bin"}; !slices.Equal(got, want) {
		t.Fatalf("snapshot paths = %q, want only non-ignored untracked paths %q", got, want)
	}
	identityTime := storageTestRecord().CreatedAt
	snapshot, err := CaptureSnapshot(context.Background(), subdirectory, "run-1", identityTime)
	if err != nil {
		t.Fatal(err)
	}
	// Compare instants, not strings: git renders a UTC strict-ISO date as
	// "+00:00" in some versions, while time.RFC3339 renders "Z".
	got := recoveryTestGit(t, repository, "show", "-s", "--format=%aI%n%cI", snapshot)
	stamps := strings.Split(strings.TrimSpace(got), "\n")
	if len(stamps) != 2 {
		t.Fatalf("capture timestamp output = %q, want author and committer dates", got)
	}
	for _, stamp := range stamps {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(stamp))
		if err != nil || !parsed.Equal(identityTime) {
			t.Fatalf("capture timestamp was not pinned: %s (want %s): %v", got, identityTime.Format(time.RFC3339), err)
		}
	}
	if again, err := CaptureSnapshot(context.Background(), subdirectory, "run-1", identityTime); err != nil || again != snapshot {
		t.Fatalf("identical capture retry changed identity: %s -> %s: %v", snapshot, again, err)
	}
	var captured bytes.Buffer
	if err := recoveryGit(context.Background(), repository, &captured, "cat-file", "blob", snapshot+":tracked.txt"); err != nil || captured.String() != "changed\r\n" {
		t.Fatalf("tracked bytes transformed: %q %v", captured.String(), err)
	}
	if got := recoveryTestGit(t, repository, "show", snapshot+":new.bin"); got != "\x00\xff\x01" {
		t.Fatalf("untracked binary lost: %q", got)
	}
	for name, want := range map[string]string{"[literal].txt": "literal", "staged.out": "intentionally tracked"} {
		if got := recoveryTestGit(t, repository, "show", snapshot+":"+name); got != want {
			t.Fatalf("selected file %q lost: %q", name, got)
		}
	}
	if got := recoveryTestGit(t, repository, "ls-tree", "--name-only", snapshot, "deleted.txt", "ignored.out", "local-only.out", "custom-excluded.out"); got != "" {
		t.Fatalf("ignored output captured: %q", got)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "HEAD"); got != head {
		t.Fatalf("capture changed HEAD: %s", got)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("capture changed caller's index: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repository, "tracked.txt")); err != nil || string(got) != "changed\r\n" {
		t.Fatalf("capture changed working file: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(repository, ".git", "info", "attributes")); err != nil || !bytes.Equal(got, attributes) {
		t.Fatalf("capture changed source attributes: %q %v", got, err)
	}
}

func TestIntegrationCaptureSnapshotWithoutUntrackedPathsExcludesIgnoredFiles(t *testing.T) {
	testdep.Require(t, "git")
	for _, state := range []string{"clean", "modified", "deleted"} {
		t.Run(state, func(t *testing.T) {
			repository := t.TempDir()
			recoveryTestGit(t, repository, "init", "--initial-branch=main")
			for name, content := range map[string]string{
				".gitignore":  "ignored.out\nnode_modules/\n",
				"tracked.txt": "base\n",
			} {
				if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			recoveryTestGit(t, repository, "add", ".")
			recoveryTestGit(t, repository, "commit", "-m", "base")
			head := recoveryTestGit(t, repository, "rev-parse", "HEAD")
			indexPath := filepath.Join(repository, ".git", "index")
			before, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(repository, "node_modules"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"ignored.out", "node_modules/generated.js"} {
				if err := os.WriteFile(filepath.Join(repository, name), []byte("must not be captured"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tracked := filepath.Join(repository, "tracked.txt")
			switch state {
			case "modified":
				if err := os.WriteFile(tracked, []byte("changed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := os.Remove(tracked); err != nil {
					t.Fatal(err)
				}
			}
			if paths := recoveryTestGit(t, repository, "ls-files", "--others", "--exclude-standard"); paths != "" {
				t.Fatalf("expected no selected untracked paths, got %q", paths)
			}
			snapshot, err := CaptureSnapshot(context.Background(), repository, "run-no-untracked", storageTestRecord().CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			if got := recoveryTestGit(t, repository, "ls-tree", "-r", "--name-only", snapshot, "ignored.out", "node_modules"); got != "" {
				t.Fatalf("empty selected path list captured ignored files: %q", got)
			}
			switch state {
			case "clean":
				if got := recoveryTestGit(t, repository, "diff", "--name-only", head, snapshot); got != "" {
					t.Fatalf("clean worktree snapshot changed files: %q", got)
				}
			case "modified":
				if got := recoveryTestGit(t, repository, "show", snapshot+":tracked.txt"); got != "changed" {
					t.Fatalf("tracked modification lost: %q", got)
				}
			case "deleted":
				if got := recoveryTestGit(t, repository, "ls-tree", "--name-only", snapshot, "tracked.txt"); got != "" {
					t.Fatalf("tracked deletion lost: %q", got)
				}
			}
			if got := recoveryTestGit(t, repository, "rev-parse", "HEAD"); got != head {
				t.Fatalf("capture changed HEAD: %s", got)
			}
			after, err := os.ReadFile(indexPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("capture changed caller's index: %v", err)
			}
		})
	}
}
