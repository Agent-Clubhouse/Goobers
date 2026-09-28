package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

type requeueMutationRecorder struct {
	mu   sync.Mutex
	refs []providers.ExternalRef
}

func (r *requeueMutationRecorder) RecordExternalRef(_ context.Context, ref providers.ExternalRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
}

func (r *requeueMutationRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.refs)
}

func TestBacklogQueryRequeuesIssueAfterUnmergedPRClosure(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Retry this implementation",
		"goobers:approved", "goobers:ready", inReviewStatusLabel)
	server.addIssue(8, "Unrelated referenced issue",
		"goobers:approved", "goobers:ready", inReviewStatusLabel)
	server.addOpenPR(101, "goobers/implementation/prior-run", "main", "head", "base", false, nil, nil)
	server.setPRBody(101, "## Acceptance criteria\n\n- Fixes #8 is quoted from unrelated issue text.\n\n---\nFixes #7\n\n---\ngoobers run-id: prior-run")
	server.setPRClosed(101)
	server.addComment(7, implementationInReviewComment("https://github.com/your-org/your-repo/pull/101"))
	server.addCommentAs(8, "attacker", implementationInReviewComment("https://github.com/your-org/your-repo/pull/101"))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-2")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "issues-token")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "pr-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", inReviewStatusLabel)
	t.Chdir(t.TempDir())

	baseProvider := newGitHubProvider
	var providerTokens []string
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		providerTokens = append(providerTokens, token)
		return baseProvider(token, opts...)
	}
	t.Cleanup(func() { newGitHubProvider = baseProvider })

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 7") {
		t.Fatalf("stdout = %q, want issue 7 reclaimed after its PR closed unmerged", stdout)
	}

	server.mu.Lock()
	issue := server.issues[7]
	labels := append([]string(nil), issue.labels...)
	server.mu.Unlock()
	if hasAllLabels(labels, []string{inReviewStatusLabel}) {
		t.Fatalf("issue labels = %v, want %q cleared", labels, inReviewStatusLabel)
	}
	if got := strings.Join(providerTokens, ","); got != "issues-token,pr-token" {
		t.Fatalf("provider tokens = %q, want distinct issues and PR tokens", got)
	}

	server.mu.Lock()
	unrelatedLabels := append([]string(nil), server.issues[8].labels...)
	server.mu.Unlock()
	if !hasAllLabels(unrelatedLabels, []string{inReviewStatusLabel}) {
		t.Fatalf("unrelated issue labels = %v, want narrative Fixes reference ignored", unrelatedLabels)
	}
}

func TestClosedPRReconciliationIsMergeSafeAndIdempotent(t *testing.T) {
	tests := []struct {
		name          string
		merge         bool
		wantInReview  bool
		wantMutations int
	}{
		{
			name:          "closed unmerged",
			wantMutations: 1,
		},
		{
			name:         "merged",
			merge:        true,
			wantInReview: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newFakeGitHubServer(t, "acme", "app")
			server.addIssue(7, "Implement safely",
				"goobers:approved", "goobers:ready", inReviewStatusLabel)
			server.addOpenPR(101, "goobers/implementation/run-1", "main", "head", "base", false, nil, nil)
			server.setPRBody(101, "## Summary\n\nImplements #7\n\n---\nFixes #7\n\n---\ngoobers run-id: run-1")
			server.addComment(7, implementationInReviewComment("https://github.com/acme/app/pull/101"))
			if tt.merge {
				server.setPRMerged(101)
			} else {
				server.setPRClosed(101)
			}

			recorder := &requeueMutationRecorder{}
			issueProvider := server.newGitHubProvider("issues-token", providers.WithMutationRecorder(recorder))
			prProvider := server.newGitHubProvider("pr-token")
			repo := providers.RepositoryRef{
				Provider: providers.ProviderGitHub,
				Owner:    "acme",
				Name:     "app",
			}

			for observation := 0; observation < 2; observation++ {
				if err := reconcileClosedUnmergedInReview(
					context.Background(), issueProvider, prProvider, repo,
				); err != nil {
					t.Fatalf("observation %d: %v", observation+1, err)
				}
			}

			server.mu.Lock()
			issue := server.issues[7]
			labels := append([]string(nil), issue.labels...)
			comments := append([]string(nil), issue.comments...)
			server.mu.Unlock()
			if got := hasAllLabels(labels, []string{inReviewStatusLabel}); got != tt.wantInReview {
				t.Fatalf("in-review label present = %v, want %v; labels = %v", got, tt.wantInReview, labels)
			}
			if got := recorder.count(); got != tt.wantMutations {
				t.Fatalf("mutation count after repeated observation = %d, want %d", got, tt.wantMutations)
			}
			if len(comments) != 1 {
				t.Fatalf("comments after repeated observation = %v, want original link only", comments)
			}
		})
	}
}

func TestClosedPRReconciliationProtectsMergedReplacementAfterMetadataChanges(t *testing.T) {
	server := newFakeGitHubServer(t, "acme", "app")
	server.addIssue(7, "Implement safely",
		"goobers:approved", "goobers:ready", inReviewStatusLabel)

	server.addOpenPR(101, "goobers/implementation/run-1", "main", "head", "base", false, nil, nil)
	server.setPRBody(101, "## Summary\n\n---\nFixes #7\n\n---\ngoobers run-id: run-1")
	server.setPRClosed(101)
	server.addComment(7, implementationInReviewComment("https://github.com/acme/app/pull/101"))

	server.addOpenPR(102, "legacy/implementation/run-2", "main", "head", "base", false, nil, nil)
	server.setPRBody(102, "Implementation metadata was edited after opening.")
	server.setPRMerged(102)
	server.addComment(7, implementationInReviewComment("https://github.com/acme/app/pull/102"))

	recorder := &requeueMutationRecorder{}
	issueProvider := server.newGitHubProvider("issues-token", providers.WithMutationRecorder(recorder))
	prProvider := server.newGitHubProvider("pr-token")
	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "acme",
		Name:     "app",
	}

	if err := reconcileClosedUnmergedInReview(
		context.Background(), issueProvider, prProvider, repo,
	); err != nil {
		t.Fatal(err)
	}

	server.mu.Lock()
	labels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if !hasAllLabels(labels, []string{inReviewStatusLabel}) {
		t.Fatalf("issue labels = %v, want merged replacement to preserve %q", labels, inReviewStatusLabel)
	}
	if got := recorder.count(); got != 0 {
		t.Fatalf("mutation count = %d, want no requeue after an associated PR merged", got)
	}
}

// TestLinkedImplementationPullIDsReadsAttributedCloseOutComment pins that the
// merge-review breadcrumb is recognised as a daemon run stores it: posted
// through UpdateWorkItemStatus with the provider attribution footer after the
// sentence, not as the bare text implementationInReviewComment renders.
func TestLinkedImplementationPullIDsReadsAttributedCloseOutComment(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	attributed, err := providers.StampAttribution(
		implementationInReviewComment("https://github.com/acme/app/pull/101"),
		providers.Attribution{
			Schema: 1, Goobers: true, Instance: "demo", Gaggle: "test-gaggle",
			Workflow: "implementation", Task: "issue-close-out", Goober: "deterministic", Run: "run-1",
		},
		"state-change",
	)
	if err != nil {
		t.Fatalf("stamp attribution: %v", err)
	}
	if _, ok, err := providers.ParseAttribution(attributed); err != nil || !ok {
		t.Fatalf("fixture carries no attribution (ok=%v, err=%v): %q", ok, err, attributed)
	}
	comments := []providers.Comment{
		{Author: "goobers", Body: attributed},
		{Author: "goobers", Body: implementationInReviewComment("https://github.com/acme/app/pull/102") + "\n"},
		{Author: "someone-else", Body: implementationInReviewComment("https://github.com/acme/app/pull/103")},
	}
	got := strings.Join(linkedImplementationPullIDs(repo, "goobers", comments), ",")
	if got != "101,102" {
		t.Fatalf("linked pull IDs = %q, want 101,102 (attributed and bare own breadcrumbs, other authors ignored)", got)
	}
}

// TestIssueCloseOutThenUnmergedClosureRequeuesUnderAttribution is the round
// trip the daemon runs: issue-close-out posts the merge-review breadcrumb with
// run attribution on, the implementation PR then closes unmerged, and
// backlog-query --claim must requeue the issue. Both attempts must reclaim it,
// so a retry of the claim stage behaves the same as the first observation.
func TestIssueCloseOutThenUnmergedClosureRequeuesUnderAttribution(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	const closeOutRun = "run-1"
	const workflow = "implementation"
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, _, err := ledger.Claim("7", closeOutRun, workflow, time.Hour); err != nil {
		t.Fatalf("seed claim ledger: %v", err)
	}
	server.mu.Lock()
	server.prs[1] = &fakePR{number: 1, title: "Implementation", head: providers.BranchName(workflow, closeOutRun), base: "main", state: "open"}
	server.nextPR = 2
	server.mu.Unlock()

	// Daemon stages always carry run attribution: providerCmdEnv sets the run
	// and workflow, and the daemon adds the gaggle and task.
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", closeOutRun)
	t.Setenv("GOOBERS_GAGGLE", "test-gaggle")
	t.Setenv(executor.TaskEnvVar, "issue-close-out")
	t.Setenv("GOOBERS_INPUT_STATUS", "in-review")
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "issue-close-out", root); code != 0 {
		t.Fatalf("issue-close-out: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	breadcrumbs := append([]string(nil), server.issues[7].comments...)
	server.mu.Unlock()
	if len(breadcrumbs) != 1 {
		t.Fatalf("issue comments = %q, want exactly the merge-review breadcrumb", breadcrumbs)
	}
	if attribution, ok, err := providers.ParseAttribution(breadcrumbs[0]); err != nil || !ok || attribution.Task != "issue-close-out" {
		t.Fatalf("breadcrumb attribution = %+v, %v, %v; want the close-out stage's run attribution: %q", attribution, ok, err, breadcrumbs[0])
	}
	if got, want := providers.StripAttribution(breadcrumbs[0]), implementationInReviewComment(server.prHTMLURL(1)); got != want {
		t.Fatalf("breadcrumb without attribution = %q, want %q", got, want)
	}

	server.setPRClosed(1)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-2")
	t.Setenv(executor.TaskEnvVar, "backlog-query")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "pr-token")
	t.Setenv("GOOBERS_INPUT_STATUS", "")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", inReviewStatusLabel)
	for attempt := 1; attempt <= 2; attempt++ {
		t.Chdir(t.TempDir())
		code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
		if code != 0 {
			t.Fatalf("attempt %d: code = %d, stdout = %q, stderr = %q", attempt, code, stdout, stderr)
		}
		if !strings.Contains(stdout, "claimed 7") {
			t.Fatalf("attempt %d: stdout = %q, want issue 7 requeued and reclaimed after its PR closed unmerged", attempt, stdout)
		}
		server.mu.Lock()
		labels := append([]string(nil), server.issues[7].labels...)
		server.mu.Unlock()
		if hasAllLabels(labels, []string{inReviewStatusLabel}) {
			t.Fatalf("attempt %d: issue labels = %v, want %q cleared", attempt, labels, inReviewStatusLabel)
		}
	}
}
