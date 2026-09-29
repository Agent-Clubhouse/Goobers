package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/providers"
)

func TestPRSelectConsumesWebhookTargetBeforePollingFallback(t *testing.T) {
	t.Run("webhook target", func(t *testing.T) {
		root := initDemo(t)
		server := newFakeGitHubServer(t, "your-org", "your-repo")
		server.addIssue(10, "Earlier PR")
		server.addOpenPR(10, "goobers/implementation/run-10", "main", "head10", "base", false, nil, nil)
		server.addIssue(11, "Webhook PR")
		server.addOpenPR(11, "goobers/implementation/run-11", "main", "head11", "base", false, nil, nil)
		providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-webhook")
		t.Setenv(executor.TriggerRefEnvVar, webhookhttp.TriggerRef(webhookhttp.Delivery{
			Event:      "pull_request",
			PullNumber: 11,
		}))

		dir := t.TempDir()
		t.Chdir(dir)
		if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
			t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := selectedPullNumber(t, filepath.Join(dir, "selected-pr.json")); got != "11" {
			t.Fatalf("selected pull request = %q, want webhook target 11", got)
		}
		if got := server.pullListRequestCount(); got != 1 {
			t.Fatalf("pull-request list calls = %d, want 1 complete foundation scan", got)
		}
	})

	t.Run("webhook target on release branch", func(t *testing.T) {
		root := initDemo(t)
		server := newFakeGitHubServer(t, "your-org", "your-repo")
		server.addIssue(12, "Release branch PR")
		server.addOpenPR(12, "goobers/implementation/run-12", "release/v0.4", "head12", "releasebase", false, nil, nil)
		providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-webhook-release")
		t.Setenv(executor.TriggerRefEnvVar, webhookhttp.TriggerRef(webhookhttp.Delivery{
			Event:      "pull_request",
			PullNumber: 12,
		}))

		dir := t.TempDir()
		t.Chdir(dir)
		if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
			t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := selectedPullNumber(t, filepath.Join(dir, "selected-pr.json")); got != "12" {
			t.Fatalf("selected pull request = %q, want release-branch webhook target 12", got)
		}
		if got := selectedPullField(t, filepath.Join(dir, "selected-pr.json"), "base"); got != "release/v0.4" {
			t.Fatalf("selected base = %q, want release/v0.4", got)
		}
	})

	t.Run("webhook target on release branch with aged fairness candidate", func(t *testing.T) {
		root := initDemo(t)
		server := newFakeGitHubServer(t, "your-org", "your-repo")
		server.addIssue(10, "Aged default branch PR")
		server.addOpenPR(10, "goobers/implementation/run-10", "main", "head10", "mainbase", false, nil, nil)
		server.addIssue(12, "Release branch PR")
		server.addOpenPR(12, "goobers/implementation/run-12", "release/v0.4", "head12", "releasebase", false, nil, nil)
		t.Setenv("GOOBERS_GAGGLE", "goobers")
		aged := providers.PullRequestSummary{Number: 10, HeadSHA: "head10"}
		if _, err := observePRSelectEligibility(
			root,
			providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"},
			[]providers.PullRequestSummary{aged},
			[]providers.PullRequestSummary{aged},
			prSelectCompleteSnapshot,
			time.Now().UTC().Add(-2*prSelectStarvationLimit),
		); err != nil {
			t.Fatal(err)
		}
		providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-webhook-release-aged")
		t.Setenv(executor.TriggerRefEnvVar, webhookhttp.TriggerRef(webhookhttp.Delivery{
			Event:      "pull_request",
			PullNumber: 12,
		}))

		dir := t.TempDir()
		t.Chdir(dir)
		if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
			t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := selectedPullNumber(t, filepath.Join(dir, "selected-pr.json")); got != "12" {
			t.Fatalf("selected pull request = %q, want release-branch webhook target 12 despite complete fairness scan", got)
		}
	})

	t.Run("webhook target on release branch respects sibling hold", func(t *testing.T) {
		root := initDemo(t)
		server := newFakeGitHubServer(t, "your-org", "your-repo")
		server.addIssue(12, "Blocked release branch PR")
		server.addOpenPR(12, "goobers/implementation/run-12", "release/v0.4", "head12", "releasebase", false, []string{blockedOnSiblingLabel}, nil)
		server.addIssue(13, "Release branch blocker")
		server.addOpenPR(13, "goobers/implementation/run-13", "release/v0.4", "head13", "releasebase", false, nil, nil)
		server.addComment(12, blockedOnSiblingCommentFor(t, 13))
		providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-webhook-release-blocked")
		t.Setenv(executor.TriggerRefEnvVar, webhookhttp.TriggerRef(webhookhttp.Delivery{
			Event:      "pull_request",
			PullNumber: 12,
		}))

		t.Chdir(t.TempDir())
		if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
			t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		} else if !strings.Contains(stdout, "blocked on sibling #13") || !strings.Contains(stdout, "no work") {
			t.Fatalf("stdout = %q, want targeted release PR held behind release sibling #13", stdout)
		}
	})

	t.Run("scheduled polling fallback", func(t *testing.T) {
		root := initDemo(t)
		server := newFakeGitHubServer(t, "your-org", "your-repo")
		server.addIssue(10, "Polling PR")
		server.addOpenPR(10, "goobers/implementation/run-10", "main", "head10", "base", false, nil, nil)
		providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-poll")

		dir := t.TempDir()
		t.Chdir(dir)
		if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
			t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := selectedPullNumber(t, filepath.Join(dir, "selected-pr.json")); got != "10" {
			t.Fatalf("selected pull request = %q, want 10", got)
		}
		if got := server.pullListRequestCount(); got != 1 {
			t.Fatalf("pull-request list calls = %d, want 1 polling fallback", got)
		}
	})
}

func TestProviderRepoPrefersRoutedRunRepository(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, "github")
	t.Setenv(executor.RepoOwnerEnvVar, "routed-owner")
	t.Setenv(executor.RepoNameEnvVar, "routed-repo")

	repo, err := providerRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if repo.Owner != "routed-owner" || repo.Name != "routed-repo" {
		t.Fatalf("providerRepo = %+v", repo)
	}
}

func selectedPullNumber(t *testing.T, path string) string {
	t.Helper()
	return selectedPullField(t, path, "number")
}

func selectedPullField(t *testing.T, path, field string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := decodePRSelectionTestResult(data, &result); err != nil {
		t.Fatal(err)
	}
	return result[field]
}
