//go:build windows

package instance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestGitSourceFallsBackToPlainTextOnWindowsSymlinkPrivilegeError(t *testing.T) {
	repo := newGitSourceTestRepo(t, "target\n")
	addGitSourceTestSymlink(t, repo, "link.txt", "config.txt")
	runGitSourceTest(t, repo, "commit", "-m", "add symlink")

	source, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   repo,
	})
	if err != nil {
		t.Fatalf("NewGitSource: %v", err)
	}
	source.createSymlink = func(string, string) error {
		return windows.ERROR_PRIVILEGE_NOT_HELD
	}

	dir, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertGitSourceTestFile(t, dir, "link.txt", "config.txt")
	info, err := os.Lstat(filepath.Join(dir, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("link.txt was materialized as a real symlink")
	}
	warnings := source.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "link.txt") {
		t.Fatalf("Warnings = %q, want single warning for link.txt", warnings)
	}

	reused, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve unchanged revision: %v", err)
	}
	if reused != dir {
		t.Fatalf("reused snapshot = %q, want %q", reused, dir)
	}
	warnings = source.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "link.txt") {
		t.Fatalf("Warnings after reused resolve = %q, want single warning for link.txt", warnings)
	}
}

func addGitSourceTestSymlink(t *testing.T, repo, name, target string) {
	t.Helper()
	linkBlob := filepath.Join(repo, name+".target")
	if err := os.WriteFile(linkBlob, []byte(target), 0o644); err != nil {
		t.Fatal(err)
	}
	objectID := strings.TrimSpace(runGitSourceTest(t, repo, "hash-object", "-w", "--", linkBlob))
	runGitSourceTest(t, repo, "update-index", "--add", "--cacheinfo", "120000,"+objectID+","+name)
}
