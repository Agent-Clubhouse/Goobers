package main

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/testdep"
	"github.com/goobers/goobers/internal/testgit"
)

// installRecordingGit puts a fake `git` on PATH that appends its argv and the
// git-relevant slice of its environment to a log file and exits 0. It lets a
// test assert the exact remote URL and credential shape the preflight hands
// git, which is the whole contract at issue: a Gitea repo must be probed at its
// own configured forge, not github.com, and with Gitea's auth shape.
func installRecordingGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "git.log")
	script := "#!" + exec.Command("bash").Path + "\n" +
		"{\n" +
		"  echo \"ARGV: $*\"\n" +
		"  for i in 0 1 2 3; do\n" +
		"    k=\"GIT_CONFIG_KEY_$i\"; v=\"GIT_CONFIG_VALUE_$i\"\n" +
		"    if [ -n \"${!k:-}\" ]; then echo \"CFG: ${!k}=${!v:-}\"; fi\n" +
		"  done\n" +
		"} >> \"$GOOBERS_TEST_GIT_LOG\"\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_TEST_GIT_LOG", logFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func readGitLog(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read git log: %v", err)
	}
	return string(raw)
}

// TestGitRepositoryReachableProbesGiteaForge is the regression test for the
// REPO001 failure: `goobers validate --check-repos` refused a Gitea repo
// outright ("provider %q does not support repository preflight"), so a
// correctly configured Gitea instance could not pass validation at all.
//
// It pins the two things that make the probe meaningful: the ls-remote must
// target the CONFIGURED forge root (never github.com), and it must carry
// Gitea's credential shape.
func TestGitRepositoryReachableProbesGiteaForge(t *testing.T) {
	testdep.Require(t, "bash")
	logFile := installRecordingGit(t)

	repo := instance.RepoRef{
		Provider: "gitea",
		Owner:    "acme",
		Name:     "widgets",
		BaseURL:  "https://gitea.example.test",
	}
	if err := gitRepositoryReachable(context.Background(), repo, "gitea-preflight-token", nil); err != nil {
		t.Fatalf("gitRepositoryReachable: %v", err)
	}

	log := readGitLog(t, logFile)
	if !strings.Contains(log, "ls-remote https://gitea.example.test/acme/widgets.git") {
		t.Fatalf("preflight did not ls-remote the configured gitea forge; log:\n%s", log)
	}
	if strings.Contains(log, "github.com") {
		t.Fatalf("gitea preflight reached github.com; log:\n%s", log)
	}

	// Gitea authenticates the token as the basic-auth USERNAME with an empty
	// password. Reusing the GitHub arm's "x-access-token:<token>" shape would
	// fail auth against a healthy forge and misreport it unreachable.
	wantAuth := base64.StdEncoding.EncodeToString([]byte("gitea-preflight-token:"))
	if !strings.Contains(log, "AUTHORIZATION: basic "+wantAuth) {
		t.Fatalf("gitea preflight did not send gitea's auth shape; log:\n%s", log)
	}
	badAuth := base64.StdEncoding.EncodeToString([]byte("x-access-token:gitea-preflight-token"))
	if strings.Contains(log, badAuth) {
		t.Fatalf("gitea preflight sent GitHub's x-access-token auth shape; log:\n%s", log)
	}
}

// TestGitRepositoryReachableKeepsGitHubProbe is the retained GitHub coverage:
// the GitHub arm must still probe github.com with its own auth shape.
func TestGitRepositoryReachableKeepsGitHubProbe(t *testing.T) {
	testdep.Require(t, "bash")
	logFile := installRecordingGit(t)

	repo := instance.RepoRef{Provider: "github", Owner: "acme", Name: "app"}
	if err := gitRepositoryReachable(context.Background(), repo, "gh-token", nil); err != nil {
		t.Fatalf("gitRepositoryReachable: %v", err)
	}

	log := readGitLog(t, logFile)
	if !strings.Contains(log, "ls-remote https://github.com/acme/app.git") {
		t.Fatalf("github preflight did not ls-remote github.com; log:\n%s", log)
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("x-access-token:gh-token"))
	if !strings.Contains(log, "AUTHORIZATION: basic "+wantAuth) {
		t.Fatalf("github preflight lost its auth shape; log:\n%s", log)
	}
}

// TestGitRepositoryReachableRejectsUnsupportedProvider keeps the explicit
// refusal for kinds that genuinely have no preflight, so adding the Gitea arm
// did not turn an unknown provider into a silent github.com probe.
func TestGitRepositoryReachableRejectsUnsupportedProvider(t *testing.T) {
	testdep.Require(t, "bash")
	logFile := installRecordingGit(t)

	repo := instance.RepoRef{Provider: "bitbucket", Owner: "acme", Name: "app"}
	err := gitRepositoryReachable(context.Background(), repo, "tok", nil)
	if err == nil {
		t.Fatal("expected an unsupported-provider error")
	}
	if !strings.Contains(err.Error(), "does not support repository preflight") {
		t.Fatalf("error = %v", err)
	}
	if raw, readErr := os.ReadFile(logFile); readErr == nil && strings.Contains(string(raw), "ls-remote") {
		t.Fatalf("unsupported provider still ran git; log:\n%s", raw)
	}
}

// TestGitRepositoryReachableRequiresGiteaBaseURL: a Gitea repo with no baseUrl
// has no forge to probe. Fail with that diagnosis rather than building a
// nonsense URL and blaming the network.
func TestGitRepositoryReachableRequiresGiteaBaseURL(t *testing.T) {
	testdep.Require(t, "bash")
	installRecordingGit(t)

	repo := instance.RepoRef{Provider: "gitea", Owner: "acme", Name: "widgets"}
	err := gitRepositoryReachable(context.Background(), repo, "tok", nil)
	if err == nil {
		t.Fatal("expected a missing-baseUrl error")
	}
	if !strings.Contains(err.Error(), "baseUrl") {
		t.Fatalf("error = %v, want a baseUrl diagnosis", err)
	}
}

// TestGiteaPreflightRootURLNormalization pins the baseUrl normalization to
// NewGiteaProvider's own RootURL derivation. An operator who writes the API
// endpoint as baseUrl (a reasonable reading) would otherwise get an ls-remote
// against <host>/api/v1/<owner>/<repo>.git and a misleading "unreachable".
func TestGiteaPreflightRootURLNormalization(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "plain root", baseURL: "https://gitea.example.test", want: "https://gitea.example.test"},
		{name: "trailing slash", baseURL: "https://gitea.example.test/", want: "https://gitea.example.test"},
		{name: "api endpoint", baseURL: "https://gitea.example.test/api/v1", want: "https://gitea.example.test"},
		{name: "api endpoint with slash", baseURL: "https://gitea.example.test/api/v1/", want: "https://gitea.example.test"},
		{name: "surrounding whitespace", baseURL: "  https://gitea.example.test  ", want: "https://gitea.example.test"},
		{name: "subpath install", baseURL: "https://example.com/gitea", want: "https://example.com/gitea"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := giteaPreflightRootURL(instance.RepoRef{
				Provider: "gitea", Owner: "o", Name: "n", BaseURL: tc.baseURL,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("root = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGitRepositoryReachableAgainstRealGiteaStyleRemote runs the preflight
// against a REAL local git remote using the real git binary, proving the
// constructed URL is one git can actually resolve refs from and that a
// genuinely missing repo is reported unreachable. It uses a file:// baseUrl so
// no network or Gitea server is required.
func TestGitRepositoryReachableAgainstRealGiteaStyleRemote(t *testing.T) {
	testdep.Require(t, "git")

	root := t.TempDir()
	// Lay the bare repo out exactly as the preflight addresses it:
	// <root>/<owner>/<name>.git
	repoDir := filepath.Join(root, "acme", "widgets.git")
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := testgit.Command(args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(root, "init", "--bare", "--quiet", repoDir)

	// Give it a ref so ls-remote returns something meaningful.
	work := filepath.Join(root, "work")
	run(root, "init", "--quiet", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "README.md")
	run(work, "-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-qm", "init")
	run(work, "remote", "add", "origin", repoDir)
	run(work, "push", "-q", "origin", "HEAD:refs/heads/main")

	t.Run("existing repo is reachable", func(t *testing.T) {
		err := gitRepositoryReachable(context.Background(), instance.RepoRef{
			Provider: "gitea", Owner: "acme", Name: "widgets", BaseURL: "file://" + root,
		}, "", nil)
		if err != nil {
			t.Fatalf("expected the real remote to be reachable: %v", err)
		}
	})

	t.Run("missing repo is unreachable", func(t *testing.T) {
		err := gitRepositoryReachable(context.Background(), instance.RepoRef{
			Provider: "gitea", Owner: "acme", Name: "does-not-exist", BaseURL: "file://" + root,
		}, "", nil)
		if err == nil {
			t.Fatal("expected a missing repo to be reported unreachable")
		}
	})
}
