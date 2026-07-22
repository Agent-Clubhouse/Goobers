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

	first, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertGitSourceTestFile(t, first, "config.txt", "main-v1\n")
	if _, err := os.Stat(filepath.Join(first, "feature-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature-only file leaked from checked-out branch: %v", err)
	}
	wantFirstRevision := strings.TrimSpace(runGitSourceTest(t, repo, "rev-parse", "main"))
	if filepath.Base(first) != wantFirstRevision {
		t.Fatalf("snapshot revision = %q, want %q", filepath.Base(first), wantFirstRevision)
	}
	reused, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve unchanged revision: %v", err)
	}
	if reused != first {
		t.Fatalf("unchanged revision resolved to %q, want %q", reused, first)
	}

	runGitSourceTest(t, repo, "restore", "config.txt")
	runGitSourceTest(t, repo, "switch", "main")
	writeGitSourceTestFile(t, repo, "config.txt", "main-v2\n")
	runGitSourceTest(t, repo, "add", "config.txt")
	runGitSourceTest(t, repo, "commit", "-m", "main v2")
	wantSecondRevision := strings.TrimSpace(runGitSourceTest(t, repo, "rev-parse", "main"))
	runGitSourceTest(t, repo, "switch", "feature")

	second, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve after main commit: %v", err)
	}
	assertGitSourceTestFile(t, second, "config.txt", "main-v2\n")
	if filepath.Base(second) != wantSecondRevision {
		t.Fatalf("snapshot revision = %q, want %q", filepath.Base(second), wantSecondRevision)
	}
	if second == first {
		t.Fatal("snapshot path did not advance after main commit")
	}
	assertGitSourceTestFile(t, first, "config.txt", "main-v1\n")
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

	first, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertGitSourceTestFile(t, first, "config.txt", "remote-v1\n")

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

	second, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve after remote update: %v", err)
	}
	assertGitSourceTestFile(t, second, "config.txt", "remote-v2\n")
	if second == first {
		t.Fatalf("snapshot did not advance after remote main update: %s", second)
	}
	assertGitSourceTestFile(t, first, "config.txt", "remote-v1\n")
}

func TestGitSourcePreservesCommittedBlobsWithArchiveAttributes(t *testing.T) {
	repo := newGitSourceTestRepo(t, "main\n")
	writeGitSourceTestFile(t, repo, ".gitattributes", "ignored.txt export-ignore\nsubstituted.txt export-subst\n")
	writeGitSourceTestFile(t, repo, "ignored.txt", "committed but export-ignored\n")
	writeGitSourceTestFile(t, repo, "substituted.txt", "$Format:%H$\n")
	runGitSourceTest(t, repo, "add", ".")
	runGitSourceTest(t, repo, "commit", "-m", "archive attributes")

	source, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   repo,
	})
	if err != nil {
		t.Fatalf("NewGitSource: %v", err)
	}
	dir, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	assertGitSourceTestFile(t, dir, "ignored.txt", "committed but export-ignored\n")
	assertGitSourceTestFile(t, dir, "substituted.txt", "$Format:%H$\n")
}

func TestGitSourceIgnoresReplacementObjects(t *testing.T) {
	repo := newGitSourceTestRepo(t, "committed\n")
	original := strings.TrimSpace(runGitSourceTest(t, repo, "rev-parse", "main:config.txt"))
	writeGitSourceTestFile(t, repo, "replacement.txt", "replacement\n")
	replacement := strings.TrimSpace(runGitSourceTest(t, repo, "hash-object", "-w", "replacement.txt"))
	runGitSourceTest(t, repo, "replace", original, replacement)

	source, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   repo,
	})
	if err != nil {
		t.Fatalf("NewGitSource: %v", err)
	}
	dir, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	assertGitSourceTestFile(t, dir, "config.txt", "committed\n")
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
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=core.fsync",
		"GIT_CONFIG_VALUE_0=none",
		"GIT_CONFIG_KEY_1=core.autocrlf",
		"GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_KEY_2=core.safecrlf",
		"GIT_CONFIG_VALUE_2=false",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
