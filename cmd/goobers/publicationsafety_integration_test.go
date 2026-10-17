//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/publicationguard"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

const safetyBranch = "goobers/implementation/run-7376"

func publicationRepo(t *testing.T) string {
	t.Helper()
	testdep.Require(t, "git")
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.name", "publication-test")
	runGitT(t, dir, "config", "user.email", "publication@example.invalid")
	runGitT(t, dir, "commit", "--allow-empty", "-m", "base")
	runGitT(t, dir, "checkout", "-b", safetyBranch)
	return dir
}

func publicationFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "add", "-f", "--", path)
	runGitT(t, dir, "commit", "-m", "change")
}

func TestIntegrationPublicationSafetyRejectsArtifactsThroughoutHistory(t *testing.T) {
	for _, path := range []string{".goobers/prompt.md", ".goober-assets/instructions.md", "mutations.jsonl", ".goobers", ".goober-assets"} {
		t.Run(path, func(t *testing.T) {
			dir := publicationRepo(t)
			publicationFile(t, dir, path, "private harness content")
			for _, deleted := range []bool{false, true} {
				if deleted {
					runGitT(t, dir, "rm", "--", path)
					runGitT(t, dir, "commit", "-m", "remove artifact")
				}
				_, err := checkPublicationSafety(t.Context(), dir, "main", safetyBranch)
				if err == nil || !strings.Contains(err.Error(), "reserved harness artifacts") {
					t.Fatalf("deleted=%v: want artifact refusal, got %v", deleted, err)
				}
			}
		})
	}
}

func TestIntegrationPublicationSafetyAllowsSourceAndCleanup(t *testing.T) {
	dir := publicationRepo(t)
	publicationFile(t, dir, ".goobers/old.log", "old artifact")
	runGitT(t, dir, "branch", "-f", "main", "HEAD")
	runGitT(t, dir, "rm", ".goobers/old.log")
	runGitT(t, dir, "commit", "-m", "remove old artifact")
	for _, path := range []string{"src/main.go", "examples/.goobers/config.json", "src/mutations.jsonl", "space and\nnewline.txt"} {
		publicationFile(t, dir, path, "ordinary repository content")
	}
	got, err := checkPublicationSafety(t.Context(), dir, "main", safetyBranch)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))
	if got != want {
		t.Fatalf("inspected tip = %q, want %q", got, want)
	}
}

func TestIntegrationPublicationSafetyRejectsRenamedArtifactAndMissingBase(t *testing.T) {
	dir := publicationRepo(t)
	publicationFile(t, dir, "ordinary.txt", "content")
	if err := os.Mkdir(filepath.Join(dir, ".goobers"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "mv", "ordinary.txt", ".goobers/renamed.log")
	runGitT(t, dir, "commit", "-m", "rename")
	if _, err := checkPublicationSafety(t.Context(), dir, "main", safetyBranch); err == nil {
		t.Fatal("rename escaped guard")
	}
	if _, err := checkPublicationSafety(t.Context(), dir, "absent", safetyBranch); err == nil {
		t.Fatal("unresolved base passed guard")
	}
	if _, err := checkPublicationSafety(t.Context(), t.TempDir(), "main", safetyBranch); err == nil {
		t.Fatal("non-repository passed guard")
	}
}

func TestIntegrationPublicationSafetyRejectsCredentialBlobsWithoutEchoing(t *testing.T) {
	for _, value := range []string{
		"ghs_" + strings.Repeat("A", 36),
		"GH_TOKEN=" + strings.Repeat("b", 24),
		`"COPILOT_CONNECTION_TOKEN": "` + strings.Repeat("c", 24) + `"`,
	} {
		dir := publicationRepo(t)
		publicationFile(t, dir, "renamed-debug.txt", value)
		runGitT(t, dir, "rm", "renamed-debug.txt")
		runGitT(t, dir, "commit", "-m", "remove log")
		_, err := checkPublicationSafety(t.Context(), dir, "main", safetyBranch)
		if err == nil || !strings.Contains(err.Error(), "credential-shaped") {
			t.Fatalf("want credential refusal, got %v", err)
		}
		if strings.Contains(err.Error(), value) {
			t.Fatal("error disclosed credential contents")
		}
	}
	for _, ordinary := range []string{"GH_TOKEN=$TOKEN", `os.Getenv("COPILOT_CONNECTION_TOKEN")`, "GH_TOKEN=[REDACTED]"} {
		if publicationguard.ContainsCredential([]byte(ordinary)) {
			t.Fatalf("ordinary reference rejected: %q", ordinary)
		}
	}
}

func TestIntegrationPushBranchPublicationSafetyRefusesBeforeRemoteWrite(t *testing.T) {
	testdep.Require(t, "git")
	origin := initBareOrigin(t)
	dir := publicationRepo(t)
	runGitT(t, dir, "remote", "add", "origin", origin)
	publicationFile(t, dir, ".goobers/debug.log", "private harness content")
	t.Setenv(executor.BaseBranchEnvVar, "main")
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "local-test-token")
	code, _, stderr := runArgs(t, "push-branch", dir)
	if code != 1 || !strings.Contains(stderr, "publication safety") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if branchExistsOnOrigin(t, origin, safetyBranch) {
		t.Fatal("unsafe branch reached origin")
	}
	if _, err := os.Stat(filepath.Join(dir, "mutations.jsonl")); !os.IsNotExist(err) {
		t.Fatal("unsafe push recorded a mutation receipt")
	}
	if err := forcePushWithLeaseWithAuth(t.Context(), dir, safetyBranch, strings.Repeat("0", 40), tokenGitAuthEnvironment("local-test-token")); err == nil || !strings.Contains(err.Error(), "publication safety") {
		t.Fatalf("force push did not refuse unsafe publication: %v", err)
	}
}

func TestIntegrationOpenPRPublicationSafetyRefusesBeforeProviderWrite(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "run-7376")
	dir := publicationRepo(t)
	publicationFile(t, dir, ".goobers/context/input.json", "harness artifact")
	t.Chdir(dir)
	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 1 || !strings.Contains(stderr, "publication safety") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.prs) != 0 {
		t.Fatal("unsafe pull request opened")
	}
}

func TestIntegrationOpenPRPublicationSafetyRejectsDifferentRemoteTip(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "run-7376")
	dir := publicationRepo(t)
	base := strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))
	publicationFile(t, dir, ".goobers/debug.log", "unsafe remote artifact")
	remoteTip := strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))
	// A local repair alone has not changed the remote branch the PR will use.
	runGitT(t, dir, "reset", "--hard", base)
	server.setBranchTip(safetyBranch, remoteTip)
	t.Chdir(dir)
	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 1 || !strings.Contains(stderr, "remote branch does not match") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.prs) != 0 {
		t.Fatal("PR opened against uninspected remote history")
	}
}

func TestIntegrationPublicationSafetyRejectsIncompleteAndMergedHistory(t *testing.T) {
	t.Run("shallow", func(t *testing.T) {
		dir := publicationRepo(t)
		base := strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "main"))
		if err := os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte(base+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := checkPublicationSafety(t.Context(), dir, "main", safetyBranch); err == nil || !strings.Contains(err.Error(), "complete local history") {
			t.Fatalf("incomplete history admitted: %v", err)
		}
	})
	t.Run("merged ancestor", func(t *testing.T) {
		dir := publicationRepo(t)
		runGitT(t, dir, "checkout", "-b", "side")
		publicationFile(t, dir, ".goobers-launcher-session-1.jsonl", "harness transcript")
		runGitT(t, dir, "rm", ".goobers-launcher-session-1.jsonl")
		runGitT(t, dir, "commit", "-m", "remove transcript")
		runGitT(t, dir, "checkout", safetyBranch)
		runGitT(t, dir, "merge", "--no-ff", "side", "-m", "merge")
		if _, err := checkPublicationSafety(t.Context(), dir, "main", safetyBranch); err == nil || !strings.Contains(err.Error(), "reserved harness artifacts") {
			t.Fatalf("unsafe merged ancestry admitted: %v", err)
		}
	})
	t.Run("reserved symlink", func(t *testing.T) {
		dir := publicationRepo(t)
		publicationFile(t, dir, "target.txt", "ordinary-target")
		blob := strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD:target.txt"))
		// Populate the symlink object without depending on host symlink privileges.
		runGitT(t, dir, "update-index", "--add", "--cacheinfo", "120000,"+blob+",.goobers")
		runGitT(t, dir, "commit", "-m", "symlink")
		if _, err := checkPublicationSafety(t.Context(), dir, "main", safetyBranch); err == nil || !strings.Contains(err.Error(), "reserved harness artifacts") {
			t.Fatalf("reserved symlink admitted: %v", err)
		}
	})
}
