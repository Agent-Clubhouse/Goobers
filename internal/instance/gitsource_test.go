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
	t.Setenv("WORKFLOW_SOURCE_TOKEN", "workflow-source-token")
	registrar := &gitSourceTestRegistrar{}

	source, err := NewWorkflowGitSource(instanceRoot, WorkflowSource{
		Kind:  WorkflowSourceKindGit,
		URL:   repositoryURL,
		Ref:   "main",
		Token: &TokenRef{Env: "WORKFLOW_SOURCE_TOKEN"},
	}, registrar)
	if err != nil {
		t.Fatalf("NewWorkflowGitSource: %v", err)
	}
	if source.local || source.mirror == "" {
		t.Fatalf("remote source = local %v, mirror %q", source.local, source.mirror)
	}

	first, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertGitSourceTestFile(t, first, "config.txt", "remote-v1\n")
	if len(registrar.values) != 1 || registrar.values[0] != "workflow-source-token" {
		t.Fatalf("registered secrets after clone = %q, want workflow-source token", registrar.values)
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

	second, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve after remote update: %v", err)
	}
	assertGitSourceTestFile(t, second, "config.txt", "remote-v2\n")
	if second == first {
		t.Fatalf("snapshot did not advance after remote main update: %s", second)
	}
	assertGitSourceTestFile(t, first, "config.txt", "remote-v1\n")
	if len(registrar.values) != 2 || registrar.values[1] != "workflow-source-token" {
		t.Fatalf("registered secrets after fetch = %q, want token resolved for each remote operation", registrar.values)
	}
}

func TestWorkflowGitSourceFailsClosedWhenTokenDoesNotResolve(t *testing.T) {
	repo := newGitSourceTestRepo(t, "remote\n")
	repoPath := filepath.ToSlash(repo)
	if runtime.GOOS == "windows" {
		repoPath = "/" + repoPath
	}
	repositoryURL := (&url.URL{Scheme: "file", Path: repoPath}).String()
	t.Setenv("EMPTY_WORKFLOW_SOURCE_TOKEN", "")

	source, err := NewWorkflowGitSource(t.TempDir(), WorkflowSource{
		Kind:  WorkflowSourceKindGit,
		URL:   repositoryURL,
		Token: &TokenRef{Env: "EMPTY_WORKFLOW_SOURCE_TOKEN"},
	}, &gitSourceTestRegistrar{})
	if err != nil {
		t.Fatalf("NewWorkflowGitSource: %v", err)
	}
	if _, err := source.Resolve(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "configrepo:read") ||
		!strings.Contains(err.Error(), "empty value") {
		t.Fatalf("Resolve error = %v, want fail-closed configrepo:read credential error", err)
	}
	if _, err := os.Stat(source.mirror); !os.IsNotExist(err) {
		t.Fatalf("managed mirror exists after credential failure: %v", err)
	}
}

func TestNewGitSourceRejectsRemoteWithoutCredentials(t *testing.T) {
	_, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   "https://example.com/workflow-config.git",
	})
	if err == nil || !strings.Contains(err.Error(), "requires configrepo:read credentials") {
		t.Fatalf("NewGitSource error = %v, want remote credential requirement", err)
	}
}

func TestGitSourceEnvExcludesAmbientCredentialRefs(t *testing.T) {
	t.Setenv("CODE_REPO_TOKEN", "code-token")
	t.Setenv("WORKFLOW_SOURCE_TOKEN", "workflow-token")
	for _, entry := range gitSourceEnv() {
		if strings.HasPrefix(entry, "CODE_REPO_TOKEN=") || strings.HasPrefix(entry, "WORKFLOW_SOURCE_TOKEN=") {
			t.Fatalf("git source inherited credential ref: %s", entry)
		}
	}
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

type gitSourceTestRegistrar struct {
	values []string
}

func (r *gitSourceTestRegistrar) Register(secret []byte) {
	r.values = append(r.values, string(secret))
}
