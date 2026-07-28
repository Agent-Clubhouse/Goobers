package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// TestOpenPRCreatesThenUpdatesOnRepass is #132's core CLI-level acceptance:
// invoking `goobers open-pr` via the actual CLI entrypoint opens a PR, writes
// prNumber/pull-request-url to the declared result file (the #132 handoff
// mechanism a downstream ci-poll stage's Task.InputsFrom consumes), and a
// second call for the same run (a repass) updates the same PR instead of
// attempting a duplicate.
func TestOpenPRCreatesThenUpdatesOnRepass(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	providerCmdEnv(t, server, "GOOBERS_CRED_PROVIDER_PR_WRITE", "run-1")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	if !strings.Contains(stdout, "pr #1") {
		t.Fatalf("stdout = %q, want a mention of the opened PR", stdout)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "pr-result.json"))
	if err != nil {
		t.Fatalf("read pr-result.json: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal pr-result.json: %v", err)
	}
	if result["prNumber"] != "1" {
		t.Fatalf("prNumber = %q, want \"1\"", result["prNumber"])
	}
	if result["pull-request-url"] == "" {
		t.Fatalf("pull-request-url missing: %+v", result)
	}

	server.mu.Lock()
	prCountAfterFirst := len(server.prs)
	server.mu.Unlock()
	if prCountAfterFirst != 1 {
		t.Fatalf("expected exactly one PR after the first call, got %d", prCountAfterFirst)
	}

	// Repass: same run, same stable branch — must update, not duplicate.
	workDir2 := t.TempDir()
	t.Chdir(workDir2)
	code, stdout, stderr = runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr (repass): code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "pr #1") {
		t.Fatalf("repass stdout = %q, want the SAME pr #1, not a new one", stdout)
	}
	server.mu.Lock()
	prCountAfterRepass := len(server.prs)
	server.mu.Unlock()
	if prCountAfterRepass != 1 {
		t.Fatalf("expected still exactly one PR after the repass, got %d (a duplicate was created)", prCountAfterRepass)
	}
}

func TestOpenPRRendersStructuredJournalBodyWithRepassHistory(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const runID = "run-rich"
	providerCmdEnv(t, server, "GOOBERS_CRED_PROVIDER_PR_WRITE", runID)

	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "query-backlog", Attempt: 1, Status: "success",
		Outputs: map[string]any{
			"id": "42", "title": "Render rich PR bodies",
			"body": "## Problem\nPR bodies lack context.\n\n### Acceptance criteria\n- [x] Include journal evidence.\n\n## Notes\nDone.",
		},
	}); err != nil {
		t.Fatalf("record claimed issue: %v", err)
	}

	recordReview := func(attempt int, verdict apiv1.Verdict, target, diff string) string {
		t.Helper()
		diffRef, recordErr := run.RecordArtifact(runID+":review/reviewer-diff.patch", []byte(diff))
		if recordErr != nil {
			t.Fatalf("record review diff: %v", recordErr)
		}
		data, marshalErr := json.Marshal(verdict)
		if marshalErr != nil {
			t.Fatalf("marshal verdict: %v", marshalErr)
		}
		verdictRef, recordErr := run.RecordArtifact("verdict/review-"+strconv.Itoa(attempt)+".json", data)
		if recordErr != nil {
			t.Fatalf("record verdict: %v", recordErr)
		}
		if appendErr := run.Append(journal.Event{
			Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(verdict.Decision), Target: target,
			Name: "verdict/review-" + strconv.Itoa(attempt) + ".json", Ref: &verdictRef,
			Runner: map[string]any{"repassAttempt": attempt, "diffDigest": diffRef.Digest},
		}); appendErr != nil {
			t.Fatalf("record review event: %v", appendErr)
		}
		return diffRef.Digest
	}

	recordReview(1, apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Summary:   "The body is incomplete.",
		Rationale: "Add local-ci evidence before opening the PR.",
	}, "implement", "diff --git a/old.go b/old.go\n--- a/old.go\n+++ b/old.go\n@@ -1 +1 @@\n-old\n+new\n")
	finalDigest := recordReview(2, apiv1.Verdict{
		Decision:  apiv1.VerdictPass,
		Summary:   "Adds a journal-backed structured PR body.",
		Rationale: "The body now gives reviewers the implementation, test, and provenance context they need.",
	}, "local-ci", "diff --git a/cmd/goobers/openpr.go b/cmd/goobers/openpr.go\n"+
		"--- a/cmd/goobers/openpr.go\n"+
		"+++ b/cmd/goobers/openpr.go\n"+
		"@@ -1 +1,2 @@\n"+
		"-old\n"+
		"+new\n"+
		"+helper\n"+
		"diff --git a/cmd/goobers/openprbody.go b/cmd/goobers/openprbody.go\n"+
		"new file mode 100644\n"+
		"--- /dev/null\n"+
		"+++ b/cmd/goobers/openprbody.go\n"+
		"@@ -0,0 +1 @@\n"+
		"+package main\n")

	stdoutRef, err := run.RecordArtifact(runID+":local-ci/stdout.log", []byte(
		"go test -race ./...\n?   \tgithub.com/goobers/goobers/cmd/empty\t[no test files]\nok  \tgithub.com/goobers/goobers/cmd/goobers\t1.234s\n",
	))
	if err != nil {
		t.Fatalf("record local-ci stdout: %v", err)
	}
	stderrRef, err := run.RecordArtifact(runID+":local-ci/stderr.log", nil)
	if err != nil {
		t.Fatalf("record local-ci stderr: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "local-ci", Attempt: 1, Status: "success",
		Artifacts: []journal.Ref{stdoutRef, stderrRef},
	}); err != nil {
		t.Fatalf("record local-ci result: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	t.Chdir(t.TempDir())
	if code, _, stderr := runArgs(t, "open-pr", root); code != 0 {
		t.Fatalf("open-pr: code = %d, stderr = %q", code, stderr)
	}

	server.mu.Lock()
	pr := server.prs[1]
	server.mu.Unlock()
	if pr == nil {
		t.Fatal("no PR opened")
	}
	for _, want := range []string{
		"## Summary",
		"Implements #42: **Render rich PR bodies**.",
		"Adds a journal-backed structured PR body.",
		"<summary>Acceptance criteria</summary>",
		"- [x] Include journal evidence.",
		"## Changes",
		"<code>cmd/goobers/openpr.go</code> (+2 / -1)",
		"<code>cmd/goobers/openprbody.go</code> (+1 / -0)",
		"**Total:** +3 / -1 across 2 file(s).",
		"## Testing",
		"**local-ci:** `success`",
		"github.com/goobers/goobers/cmd/goobers",
		"## Reviewer verdict",
		"**Decision:** `pass`",
		"The body now gives reviewers",
		"Review -&gt; repass history (2 attempts)",
		"`needs-changes` -> `implement`",
		"Add local-ci evidence before opening the PR.",
		"Fixes #42",
		finalDigest,
		"goobers run-id: " + runID,
	} {
		if !strings.Contains(pr.body, want) {
			t.Errorf("PR body missing %q:\n%s", want, pr.body)
		}
	}
	if strings.Contains(pr.body, "Automated PR opened") {
		t.Fatalf("PR body retained boilerplate:\n%s", pr.body)
	}
}

func TestParseUnifiedDiffDistinguishesHeadersFromHunkLines(t *testing.T) {
	diff := []byte("diff --git a/example.txt b/example.txt\n" +
		"--- a/example.txt\n" +
		"+++ b/example.txt\n" +
		"@@ -1 +1 @@\n" +
		"--- old\n" +
		"+++ new\n")

	changes := parseUnifiedDiff(diff)
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want one changed file", changes)
	}
	if got, want := changes[0], (prBodyChange{path: "example.txt", additions: 1, deletions: 1}); got != want {
		t.Fatalf("change = %+v, want %+v", got, want)
	}
}

func TestOpenPRFailsClosedOnMalformedExistingJournal(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const runID = "run-malformed"
	providerCmdEnv(t, server, "GOOBERS_CRED_PROVIDER_PR_WRITE", runID)
	if err := os.MkdirAll(filepath.Join(layoutFor(root).RunsDir(), runID), 0o755); err != nil {
		t.Fatalf("create malformed run directory: %v", err)
	}

	t.Chdir(t.TempDir())
	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 for malformed existing journal, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "render pull request body from journal") {
		t.Fatalf("stderr = %q, want journal rendering failure", stderr)
	}
}

// TestOpenPRMissingRunIDFailsClosed proves open-pr refuses to run without a
// real run identity — no meaningful branch/PR to open otherwise.
func TestOpenPRMissingRunIDFailsClosed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })
	t.Setenv("GOOBERS_CRED_PROVIDER_PR_WRITE", "test-token")
	// #321: a live local-ci `go test ./...` inherits the run's real
	// GOOBERS_RUN_ID/GOOBERS_WORKFLOW from buildStageEnv, defeating this
	// fail-closed test. Simulate the parent-process leak, then clear it —
	// genuinely exercises the missing-run-context path and regression-guards the
	// fix under normal CI.
	t.Setenv("GOOBERS_RUN_ID", "ambient-parent-leak")
	t.Setenv("GOOBERS_WORKFLOW", "ambient-parent-leak")
	unsetRunContext(t)
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on missing run context), stderr = %q", code, stderr)
	}

	if !strings.Contains(stderr, "GOOBERS_RUN_ID") {
		t.Fatalf("stderr = %q, want a clear missing-run-id message", stderr)
	}
}

func TestProviderWorkItemClosedMapsADOStates(t *testing.T) {
	for _, test := range []struct {
		state  string
		closed bool
	}{
		{state: "New"},
		{state: "Active"},
		{state: "In Progress"},
		{state: "Resolved", closed: true},
		{state: "Closed", closed: true},
		{state: "Removed", closed: true},
	} {
		t.Run(test.state, func(t *testing.T) {
			item := providers.WorkItem{Provider: providers.ProviderADO, State: test.state}
			if got := providerWorkItemClosed(item); got != test.closed {
				t.Fatalf("providerWorkItemClosed(%q) = %t, want %t", test.state, got, test.closed)
			}
		})
	}
}

func TestOpenPRUsesConfiguredADOIdentityAndTargetBranch(t *testing.T) {
	root := initDemo(t)
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = []instance.RepoRef{{
		Provider: "ado",
		Owner:    "organization",
		Project:  "project",
		Name:     "repository",
		Auth:     &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI},
	}}
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	var (
		body         map[string]interface{}
		opened       bool
		closePatches int
	)
	pullPath := "/organization/project/_apis/git/repositories/repository/pullrequests/42"
	mux := http.NewServeMux()
	mux.HandleFunc("/organization/project/_apis/git/repositories/repository/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			value := []interface{}{}
			if opened {
				value = append(value, map[string]interface{}{
					"pullRequestId": 42, "status": "active", "description": "Implements #772",
					"sourceRefName":         "refs/heads/goobers/implementation/run-ado",
					"targetRefName":         "refs/heads/trunk",
					"lastMergeSourceCommit": map[string]string{"commitId": "ado-head"},
					"lastMergeTargetCommit": map[string]string{"commitId": "ado-base"},
				})
			}
			if err := json.NewEncoder(w).Encode(map[string]interface{}{"value": value}); err != nil {
				t.Fatal(err)
			}
		case http.MethodPost:
			opened = true
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"pullRequestId": 42,
				"_links":        map[string]interface{}{"web": map[string]string{"href": "https://example.test/pr/42"}},
			}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})
	mux.HandleFunc(pullPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeFakeJSON(w, map[string]interface{}{"pullRequestId": 42, "status": "active"})
		case http.MethodPatch:
			closePatches++
			writeFakeJSON(w, map[string]interface{}{"pullRequestId": 42, "status": "abandoned"})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})
	mux.HandleFunc(pullPath+"/iterations", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"value": []map[string]int{{"id": 1}}})
	})
	mux.HandleFunc(pullPath+"/iterations/1/changes", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"changeEntries": []interface{}{}})
	})
	mux.HandleFunc(pullPath+"/threads", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	previousADOProvider := newADOProvider
	newADOProvider = func(organization, project, token string, opts ...func(*providers.ADOProvider)) *providers.ADOProvider {
		if token != "" {
			t.Fatalf("ADO provider token = %q, want configured credential source", token)
		}
		if len(opts) == 0 {
			t.Fatal("ADO provider was not given a configured credential source")
		}
		opts = append(opts, providers.WithADOCredentialSource(providers.NewADOPATCredentialSource("goobers", "test-token")), func(p *providers.ADOProvider) {
			p.BaseURL = server.URL
		})
		return providers.NewADOProvider(organization, project, "", opts...)
	}
	t.Cleanup(func() { newADOProvider = previousADOProvider })

	t.Setenv("GOOBERS_RUN_ID", "run-ado")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderADO))
	t.Setenv(executor.RepoOwnerEnvVar, "organization")
	t.Setenv(executor.RepoProjectEnvVar, "project")
	t.Setenv(executor.RepoNameEnvVar, "repository")
	t.Setenv(executor.RepoBranchEnvVar, "trunk")
	t.Setenv("GOOBERS_CRED_PROVIDER_PR_WRITE", "")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stderr = %q", code, stderr)
	}
	if body["targetRefName"] != "refs/heads/trunk" {
		t.Fatalf("targetRefName = %q, want configured default branch", body["targetRefName"])
	}

	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "42")
	seedGateVerdictJournal(t, root, "run-ado", apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges, Rationale: "superseded",
		HeadSHA: "ado-head", BaseSHA: "ado-base",
	})
	code, _, stderr = runArgs(t, "apply-verdict", root)
	if code != 0 || closePatches != 1 {
		t.Fatalf("ADO apply-verdict close: code=%d patches=%d stderr=%q", code, closePatches, stderr)
	}
}

func TestPullRequestProviderTokenDoesNotUseGitHubGrantForADO(t *testing.T) {
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "github-token")
	if _, err := pullRequestProviderToken(providers.RepositoryRef{Provider: providers.ProviderADO}); err == nil ||
		!strings.Contains(err.Error(), "GOOBERS_CRED_PROVIDER_PR_WRITE") {
		t.Fatalf("pullRequestProviderToken error = %v, want missing provider-neutral credential", err)
	}
}

func TestPullRequestProviderTokenAcceptsLegacyGitHubGrant(t *testing.T) {
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "github-token")
	token, err := pullRequestProviderToken(providers.RepositoryRef{Provider: providers.ProviderGitHub})
	if err != nil || token != "github-token" {
		t.Fatalf("pullRequestProviderToken = %q, %v; want legacy GitHub token", token, err)
	}
}

func TestApplyVerdictGitHubProviderUsesGitHubCredential(t *testing.T) {
	t.Setenv("GOOBERS_CRED_PROVIDER_PR_WRITE", "provider-token")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "github-token")

	provider, err := newApplyVerdictProviderForRepo(t.TempDir(), providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "acme",
		Name:     "app",
	})
	if err != nil {
		t.Fatalf("newApplyVerdictProviderForRepo: %v", err)
	}
	githubProvider, ok := provider.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("provider = %T, want *providers.GitHubProvider", provider)
	}
	if githubProvider.Token != "github-token" {
		t.Fatalf("GitHub apply-verdict token = %q, want github-specific credential", githubProvider.Token)
	}
}
