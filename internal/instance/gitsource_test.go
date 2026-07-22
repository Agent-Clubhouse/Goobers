package instance

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGitSourceLocalTracksCommittedMain(t *testing.T) {
	repo := newGitSourceTestRepo(t, "main-v1\n")
	runGitSourceTest(t, repo, "switch", "-c", "feature")
	writeGitSourceTestFile(t, repo, "config.txt", "feature\n")
	writeGitSourceTestFile(t, repo, "feature-only.txt", "feature\n")
	runGitSourceTest(t, repo, "add", ".")
	runGitSourceTest(t, repo, "commit", "-m", "feature")
	writeGitSourceTestFile(t, repo, "config.txt", "uncommitted feature edit\n")

	source, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   repo,
	})
	if err != nil {
		t.Fatalf("NewGitSource: %v", err)
	}
	if !source.local || source.mirror != "" {
		t.Fatalf("local source = %v, mirror = %q; want direct local ref access", source.local, source.mirror)
	}

	first, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	assertGitSourceTestFile(t, first.Dir, "config.txt", "main-v1\n")
	if _, err := os.Stat(filepath.Join(first.Dir, "feature-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature-only file leaked from checked-out branch: %v", err)
	}
	wantFirstRevision := strings.TrimSpace(runGitSourceTest(t, repo, "rev-parse", "main"))
	if first.Revision != wantFirstRevision {
		t.Fatalf("revision = %q, want %q", first.Revision, wantFirstRevision)
	}
	firstDir := first.Dir
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := os.Stat(firstDir); !os.IsNotExist(err) {
		t.Fatalf("closed snapshot still exists: %v", err)
	}

	runGitSourceTest(t, repo, "restore", "config.txt")
	runGitSourceTest(t, repo, "switch", "main")
	writeGitSourceTestFile(t, repo, "config.txt", "main-v2\n")
	runGitSourceTest(t, repo, "add", "config.txt")
	runGitSourceTest(t, repo, "commit", "-m", "main v2")
	wantSecondRevision := strings.TrimSpace(runGitSourceTest(t, repo, "rev-parse", "main"))
	runGitSourceTest(t, repo, "switch", "feature")

	second, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot after main commit: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("Close second snapshot: %v", err)
		}
	})
	assertGitSourceTestFile(t, second.Dir, "config.txt", "main-v2\n")
	if second.Revision != wantSecondRevision {
		t.Fatalf("revision = %q, want %q", second.Revision, wantSecondRevision)
	}
}

func TestGitSourceRemoteClonesManagedMirrorAndFetchesMain(t *testing.T) {
	repo := newGitSourceTestRepo(t, "remote-v1\n")
	instanceRoot := t.TempDir()
	repoPath := filepath.ToSlash(repo)
	if runtime.GOOS == "windows" {
		repoPath = "/" + repoPath
	}
	repositoryURL := (&url.URL{Scheme: "file", Path: repoPath}).String()

	source, err := NewGitSource(GitSourceOptions{
		InstanceRoot: instanceRoot,
		Repository:   repositoryURL,
		Ref:          "main",
	})
	if err != nil {
		t.Fatalf("NewGitSource: %v", err)
	}
	if source.local || source.mirror == "" {
		t.Fatalf("remote source = local %v, mirror %q", source.local, source.mirror)
	}

	first, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	assertGitSourceTestFile(t, first.Dir, "config.txt", "remote-v1\n")
	firstRevision := first.Revision
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := strings.TrimSpace(runGitSourceTest(t, "", "--git-dir="+source.mirror, "rev-parse", "--is-bare-repository")); got != "true" {
		t.Fatalf("managed repository is bare = %q, want true", got)
	}
	if rel, err := filepath.Rel(instanceRoot, source.mirror); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("managed mirror %q is not beneath instance root %q", source.mirror, instanceRoot)
	}
	if _, err := os.Stat(filepath.Join(source.mirror, "config.txt")); !os.IsNotExist(err) {
		t.Fatalf("managed mirror unexpectedly has a working tree: %v", err)
	}

	writeGitSourceTestFile(t, repo, "config.txt", "remote-v2\n")
	runGitSourceTest(t, repo, "add", "config.txt")
	runGitSourceTest(t, repo, "commit", "-m", "remote v2")

	second, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot after remote update: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("Close second snapshot: %v", err)
		}
	})
	assertGitSourceTestFile(t, second.Dir, "config.txt", "remote-v2\n")
	if second.Revision == firstRevision {
		t.Fatalf("revision did not advance after remote main update: %s", second.Revision)
	}
}

func TestNewGitSourceRejectsNonBranchRef(t *testing.T) {
	_, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   newGitSourceTestRepo(t, "main\n"),
		Ref:          "refs/tags/release",
	})
	if err == nil || !strings.Contains(err.Error(), "not a branch") {
		t.Fatalf("NewGitSource error = %v, want non-branch rejection", err)
	}
}

func newGitSourceTestRepo(t *testing.T, content string) string {
	t.Helper()
	repo := t.TempDir()
	runGitSourceTest(t, repo, "init", "-b", "main")
	runGitSourceTest(t, repo, "config", "user.email", "test@example.com")
	runGitSourceTest(t, repo, "config", "user.name", "Test")
	writeGitSourceTestFile(t, repo, "config.txt", content)
	runGitSourceTest(t, repo, "add", "config.txt")
	runGitSourceTest(t, repo, "commit", "-m", "initial")
	return repo
}

func writeGitSourceTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertGitSourceTestFile(t *testing.T, root, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

func runGitSourceTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsync", "GIT_CONFIG_VALUE_0=none")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
