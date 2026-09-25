//go:build liveadowrite

// Live WRITE leg for the Azure DevOps provider (ADO-N16, design
// docs/design/ado-parity-dsl-2-0.md §8.2). It mutates a dedicated scratch
// repository and its project's work items, so it sits behind its own build tag
// AND two environment opt-ins: without GOOBERS_ADO_LIVE_WRITE_REPO and
// GOOBERS_ADO_LIVE_TOKEN every test skips, which keeps any
// -tags=liveadowrite sweep without credentials inert.
//
// Rules the leg keeps (§8.2):
//
//   - Scratch repo only. Policies are created once by
//     `go run ./test/adolive provision`; tests only read them.
//   - Namespaced. Branches live under goobers-live/<run_id>/, everything
//     created is tagged goobers-live, and PR and work-item bodies carry a
//     run-id footer.
//   - Idempotent. Every create is find-or-create keyed on the run id, so a
//     re-run with the same GOOBERS_ADO_LIVE_RUN_ID converges.
//   - Cleanup. The run's PRs are abandoned, its work items moved to Removed
//     (or Closed), and only its own branches deleted. The janitor abandons
//     other runs' goobers-live PRs older than 24 hours. Nothing shared — the
//     repository, policies, tag definitions, other runs' items — is deleted.
//
// The selection rules those guarantees rest on are pure functions in
// ado_live_write_plan_test.go, unit-tested in ordinary CI. Scenarios for
// behavior not yet on main are added by the PR that ships the behavior.
//
// Run locally (GOOBERS_ADO_LIVE_WRITE_REPO is organization/project/repository):
//
//	GOOBERS_ADO_LIVE_WRITE_REPO=example-org/example-project/example-scratch \
//	GOOBERS_ADO_LIVE_TOKEN=... \
//	  go test ./providers/ -tags liveadowrite -run TestLiveADOWrite -v -count=1
//
// Optional: GOOBERS_ADO_LIVE_RUN_ID (default local-<timestamp>),
// GOOBERS_ADO_LIVE_WRITE_BASE (default main), and
// GOOBERS_ADO_LIVE_WORK_ITEM_TYPE (default: the provider's default type).
package providers

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

const (
	adoLiveStatusGenre = "goobers-live"
	adoLiveStatusName  = "live-write"
	adoLiveCallTimeout = 3 * time.Minute
)

type adoLiveWriteEnv struct {
	provider *ADOProvider
	repo     RepositoryRef
	base     string
	ns       adoLiveNamespace
}

func adoLiveWriteSetup(t *testing.T) adoLiveWriteEnv {
	t.Helper()
	testdep.RequireEnv(t, "GOOBERS_ADO_LIVE_WRITE_REPO")
	testdep.RequireEnv(t, "GOOBERS_ADO_LIVE_TOKEN")
	target := os.Getenv("GOOBERS_ADO_LIVE_WRITE_REPO")
	parts := strings.Split(target, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("GOOBERS_ADO_LIVE_WRITE_REPO = %q, want organization/project/repository", target)
	}
	ns, err := newADOLiveNamespace(adoLiveRunID(os.Getenv, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(os.Getenv("GOOBERS_ADO_LIVE_WRITE_BASE"))
	if base == "" {
		base = "main"
	}
	provider := NewADOProvider(parts[0], parts[1], os.Getenv("GOOBERS_ADO_LIVE_TOKEN"),
		WithADOSecretRegistrar(journal.NewRegistryScrubber()))
	t.Logf("live write leg: run id %s, base %s", ns.runID, base)
	return adoLiveWriteEnv{
		provider: provider,
		repo:     RepositoryRef{Provider: ProviderADO, Owner: parts[0], Project: parts[1], Name: parts[2]},
		base:     base,
		ns:       ns,
	}
}

func adoLiveContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), adoLiveCallTimeout)
	t.Cleanup(cancel)
	return ctx
}

// TestLiveADOWriteJanitor abandons other runs' live pull requests older than
// adoLiveJanitorAge. It only abandons: branches and work items of other runs
// are never touched.
func TestLiveADOWriteJanitor(t *testing.T) {
	env := adoLiveWriteSetup(t)
	ctx := adoLiveContext(t)
	prs, err := env.provider.ListPullRequests(ctx, ListPullRequestsRequest{
		Repository: env.repo, HeadPrefix: adoLiveBranchRoot, SkipCheckState: true,
	})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	for _, pr := range adoLiveJanitorPullRequests(env.ns, prs, time.Now()) {
		if _, err := env.provider.ClosePullRequest(ctx, ClosePullRequestRequest{Repository: env.repo, PullID: pr.ID}); err != nil {
			t.Errorf("janitor: abandon stale live PR %s (%s): %v", pr.ID, pr.Head, err)
			continue
		}
		t.Logf("janitor: abandoned stale live PR %s (%s, created %s)", pr.ID, pr.Head, pr.UpdatedAt.Format(time.RFC3339))
	}
}

// TestLiveADOWritePullRequest drives one run-namespaced pull request through
// find-or-create, labels, threads, statuses, the description cap and policy
// detection, then abandons it and deletes its branch.
func TestLiveADOWritePullRequest(t *testing.T) {
	env := adoLiveWriteSetup(t)
	ctx := adoLiveContext(t)
	branch := env.ns.branch("pr")
	t.Cleanup(func() { env.cleanupPullRequests(t, branch) })
	env.ensureBranch(ctx, t, branch)

	req := PullRequestRequest{
		Repository: env.repo,
		Title:      "[goobers-live] provider write leg " + env.ns.runID + " (safe to abandon)",
		Body:       "Automated Azure DevOps provider write leg. Never merged; abandoned by its own cleanup.",
		Head:       branch,
		Base:       env.base,
		Draft:      true,
		RunID:      adoLiveTag + "-" + env.ns.runID,
	}
	first, err := env.provider.OpenPullRequest(ctx, req)
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	t.Logf("pull request %s: %s", first.ID, first.URL)
	// A second open for the same source branch converges on the same PR.
	second, err := env.provider.OpenPullRequest(ctx, req)
	if err != nil {
		t.Fatalf("OpenPullRequest (re-open): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-open created PR %s, want the existing PR %s", second.ID, first.ID)
	}

	t.Run("description cap", func(t *testing.T) { env.checkDescriptionCap(ctx, t, req, first.ID) })
	t.Run("labels", func(t *testing.T) { env.checkLabels(ctx, t, first.ID) })
	t.Run("threads", func(t *testing.T) { env.checkThreads(ctx, t, first.ID) })
	t.Run("status", func(t *testing.T) { env.checkStatus(ctx, t, first.ID) })
	t.Run("policy detection", func(t *testing.T) { env.checkPolicyDetected(ctx, t) })
}

// TestLiveADOWriteWorkItem creates one run-namespaced work item through
// find-or-create, races two claims on it, closes it through the status
// mirror, and retires it to Removed (or Closed) on cleanup.
func TestLiveADOWriteWorkItem(t *testing.T) {
	env := adoLiveWriteSetup(t)
	ctx := adoLiveContext(t)
	const scenario = "wi"
	req := CreateWorkItemRequest{
		Repository: env.repo,
		Type:       strings.TrimSpace(os.Getenv("GOOBERS_ADO_LIVE_WORK_ITEM_TYPE")),
		Title:      "[goobers-live] provider write leg " + env.ns.runID + " (safe to remove)",
		Body:       "Automated Azure DevOps provider write leg work item. Retired by its own cleanup.",
		Labels:     []string{adoLiveTag},
		RunID:      env.ns.itemRunID(scenario),
	}
	item, err := env.provider.CreateWorkItem(ctx, req)
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	t.Cleanup(func() { env.retireWorkItem(t, item.ID, scenario) })
	t.Logf("work item %s (%s)", item.ID, item.Type)
	again, err := env.provider.CreateWorkItem(ctx, req)
	if err != nil {
		t.Fatalf("CreateWorkItem (re-create): %v", err)
	}
	if again.ID != item.ID {
		t.Fatalf("re-create filed work item %s, want the existing item %s", again.ID, item.ID)
	}
	if !env.ns.ownsItemBody(item.Body, scenario) {
		t.Fatalf("work item %s body lacks this run's footer", item.ID)
	}
	if !item.HasLabel(adoLiveTag) {
		t.Errorf("work item %s labels = %v, want %q", item.ID, item.Labels, adoLiveTag)
	}

	t.Run("claim race", func(t *testing.T) { env.checkClaimRace(ctx, t, item.ID) })
	t.Run("close", func(t *testing.T) { env.checkClose(ctx, t, item.ID) })
}

// ensureBranch finds or creates branch from the base tip and gives it one
// commit, so a re-run reuses a branch an earlier attempt already seeded.
func (e adoLiveWriteEnv) ensureBranch(ctx context.Context, t *testing.T, branch string) {
	t.Helper()
	if !e.ns.ownsBranch(branch) {
		t.Fatalf("refusing to write branch %q outside this run's namespace", branch)
	}
	baseSHA, err := e.provider.branchSHA(ctx, e.repo, e.base)
	if err != nil {
		t.Fatalf("resolve base %s: %v", e.base, err)
	}
	tip, found, err := e.provider.lookupBranchSHA(ctx, e.repo, branch)
	if err != nil {
		t.Fatalf("lookup %s: %v", branch, err)
	}
	if !found {
		created, err := e.provider.CreateBranch(ctx, BranchRequest{Repository: e.repo, Name: branch, BaseSHA: baseSHA})
		if err != nil {
			t.Fatalf("CreateBranch %s: %v", branch, err)
		}
		tip = created.SHA
	}
	if tip != baseSHA {
		return
	}
	if _, err := e.provider.Commit(ctx, CommitRequest{
		Repository: e.repo,
		Branch:     branch,
		BaseSHA:    tip,
		Message:    "goobers-live: seed " + branch,
		Files: []CommitFile{{
			Path:    adoLiveBranchRoot + e.ns.runID + ".md",
			Content: "Live write leg run " + e.ns.runID + ". Safe to delete.\n",
		}},
	}); err != nil {
		t.Fatalf("Commit on %s: %v", branch, err)
	}
}

func (e adoLiveWriteEnv) checkDescriptionCap(ctx context.Context, t *testing.T, req PullRequestRequest, pullID string) {
	// ADO rejects a description over adoMaxPRDescriptionChars; the provider
	// trims the body so an oversized one still lands with its footer.
	req.Body = strings.Repeat("goobers live write leg description cap.\n", 150)
	if n := len(req.Body); n <= adoMaxPRDescriptionChars {
		t.Fatalf("oversized body is only %d characters", n)
	}
	updated, err := e.provider.OpenPullRequest(ctx, req)
	if err != nil {
		t.Fatalf("OpenPullRequest with an oversized body: %v", err)
	}
	if updated.ID != pullID {
		t.Fatalf("oversized update opened PR %s, want %s", updated.ID, pullID)
	}
}

func (e adoLiveWriteEnv) checkLabels(ctx context.Context, t *testing.T, pullID string) {
	const mixed, colon = "Goobers-Live-Mixed", "goobers-live:check"
	if err := e.provider.AddPullRequestLabels(ctx, e.repo, pullID, []string{adoLiveTag, mixed, colon}); err != nil {
		t.Fatalf("AddPullRequestLabels: %v", err)
	}
	e.requireLabels(ctx, t, pullID, []string{adoLiveTag, strings.ToLower(mixed), colon}, nil)
	// Removal matches case-insensitively, and a colon-bearing label is
	// removed by id (ADO's delete-by-name rejects it).
	for _, name := range []string{strings.ToLower(mixed), colon} {
		if err := e.provider.RemovePullRequestLabel(ctx, e.repo, pullID, name); err != nil {
			t.Fatalf("RemovePullRequestLabel %q: %v", name, err)
		}
	}
	e.requireLabels(ctx, t, pullID, []string{adoLiveTag}, []string{strings.ToLower(mixed), colon})
	if err := e.provider.RemovePullRequestLabel(ctx, e.repo, pullID, colon); err != nil {
		t.Fatalf("RemovePullRequestLabel of an absent label must be benign: %v", err)
	}
}

func (e adoLiveWriteEnv) requireLabels(ctx context.Context, t *testing.T, pullID string, present, absent []string) {
	t.Helper()
	names, err := e.provider.PullRequestLabelNames(ctx, e.repo, pullID)
	if err != nil {
		t.Fatalf("PullRequestLabelNames: %v", err)
	}
	for _, name := range present {
		if !slices.Contains(names, name) {
			t.Errorf("labels %v lack %q", names, name)
		}
	}
	for _, name := range absent {
		if slices.Contains(names, name) {
			t.Errorf("labels %v still carry %q", names, name)
		}
	}
}

func (e adoLiveWriteEnv) checkThreads(ctx context.Context, t *testing.T, pullID string) {
	body := "goobers-live thread " + e.ns.runID
	posted, err := e.provider.PostPullRequestThreadComment(ctx, e.repo, pullID, body)
	if err != nil {
		t.Fatalf("PostPullRequestThreadComment: %v", err)
	}
	e.requireComment(ctx, t, pullID, posted.ID, body)
	edited := body + " (edited)"
	if err := e.provider.UpdatePullRequestThreadComment(ctx, e.repo, posted.ID, edited); err != nil {
		t.Fatalf("UpdatePullRequestThreadComment: %v", err)
	}
	e.requireComment(ctx, t, pullID, posted.ID, edited)
}

func (e adoLiveWriteEnv) requireComment(ctx context.Context, t *testing.T, pullID, commentID, body string) {
	t.Helper()
	comments, err := e.provider.ListPullRequestThreadComments(ctx, e.repo, pullID)
	if err != nil {
		t.Fatalf("ListPullRequestThreadComments: %v", err)
	}
	for _, comment := range comments {
		if comment.ID == commentID {
			if !strings.Contains(comment.Body, body) {
				t.Fatalf("comment %s body = %q, want it to contain %q", commentID, comment.Body, body)
			}
			return
		}
	}
	t.Fatalf("comment %s not listed among %d thread comments", commentID, len(comments))
}

func (e adoLiveWriteEnv) checkStatus(ctx context.Context, t *testing.T, pullID string) {
	status, err := e.provider.PublishPullRequestStatus(ctx, PullRequestStatusRequest{
		Repository:  e.repo,
		PullID:      pullID,
		Genre:       adoLiveStatusGenre,
		Name:        adoLiveStatusName,
		State:       CheckStatePassing,
		Description: "goobers live write leg " + e.ns.runID,
	})
	if err != nil {
		t.Fatalf("PublishPullRequestStatus: %v", err)
	}
	if status.ID <= 0 {
		t.Fatalf("PublishPullRequestStatus returned id %d", status.ID)
	}
}

// checkPolicyDetected reads the policies the provisioning tool created on the
// base branch. A policy-free base means the scratch repo was never provisioned.
func (e adoLiveWriteEnv) checkPolicyDetected(ctx context.Context, t *testing.T) {
	got, err := e.provider.DetectMergePolicy(ctx, RepoMergePolicyRequest{Repository: e.repo, Branch: e.base})
	if err != nil {
		t.Fatalf("DetectMergePolicy: %v", err)
	}
	if got.Policy != MergePolicyMergeQueue {
		t.Fatalf("DetectMergePolicy(%s) = %q, want %q: provision the scratch repository with `go run ./test/adolive provision -apply`",
			e.base, got.Policy, MergePolicyMergeQueue)
	}
}

func (e adoLiveWriteEnv) checkClaimRace(ctx context.Context, t *testing.T, id string) {
	winner, loser := e.ns.itemRunID("claim-a"), e.ns.itemRunID("claim-b")
	first, err := e.provider.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: e.repo, ID: id, RunID: winner})
	if err != nil {
		t.Fatalf("ClaimWorkItem (first): %v", err)
	}
	if !first.Claimed {
		t.Fatalf("first claim lost to %q", first.ClaimedBy)
	}
	second, err := e.provider.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: e.repo, ID: id, RunID: loser})
	if err != nil {
		t.Fatalf("ClaimWorkItem (second): %v", err)
	}
	if second.Claimed || second.ClaimedBy != winner {
		t.Fatalf("second claim = claimed %v by %q, want a loss to %q", second.Claimed, second.ClaimedBy, winner)
	}
	if _, err := e.provider.ReleaseWorkItemClaim(ctx, ClaimWorkItemRequest{Repository: e.repo, ID: id, RunID: winner}); err != nil {
		t.Fatalf("ReleaseWorkItemClaim: %v", err)
	}
}

func (e adoLiveWriteEnv) checkClose(ctx context.Context, t *testing.T, id string) {
	if _, err := e.provider.UpdateWorkItemStatus(ctx, UpdateWorkItemStatusRequest{Repository: e.repo, ID: id, Status: WorkItemStatusInProgress}); err != nil {
		t.Fatalf("UpdateWorkItemStatus in-progress: %v", err)
	}
	closed, err := e.provider.UpdateWorkItemStatus(ctx, UpdateWorkItemStatusRequest{Repository: e.repo, ID: id, Status: WorkItemStatusDone})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus done: %v", err)
	}
	if closed.State != "closed" {
		t.Fatalf("work item %s state = %q after done, want closed", id, closed.State)
	}
}

// cleanupPullRequests abandons this run's pull requests and deletes branch.
// Both steps re-check ownership, so a namespacing bug cannot reach another
// run's objects.
func (e adoLiveWriteEnv) cleanupPullRequests(t *testing.T, branch string) {
	ctx, cancel := context.WithTimeout(context.Background(), adoLiveCallTimeout)
	defer cancel()
	prs, err := e.provider.ListPullRequests(ctx, ListPullRequestsRequest{
		Repository: e.repo, HeadPrefix: e.ns.branchPrefix(), SkipCheckState: true,
	})
	if err != nil {
		t.Errorf("cleanup: list PRs: %v (the janitor abandons them after %s)", err, adoLiveJanitorAge)
	}
	for _, pr := range adoLiveOwnPullRequests(e.ns, prs) {
		if _, err := e.provider.ClosePullRequest(ctx, ClosePullRequestRequest{Repository: e.repo, PullID: pr.ID}); err != nil {
			t.Errorf("cleanup: abandon PR %s: %v", pr.ID, err)
		}
	}
	if !e.ns.ownsBranch(branch) {
		t.Errorf("cleanup: refusing to delete branch %q outside this run's namespace", branch)
		return
	}
	if _, err := e.provider.DeleteBranch(ctx, DeleteBranchRequest{Repository: e.repo, Name: branch}); err != nil {
		t.Errorf("cleanup: delete branch %s: %v", branch, err)
	}
}

// retireWorkItem moves this run's work item to its type's Removed state, or
// Completed where the type has none. It never deletes the item.
func (e adoLiveWriteEnv) retireWorkItem(t *testing.T, id, scenario string) {
	ctx, cancel := context.WithTimeout(context.Background(), adoLiveCallTimeout)
	defer cancel()
	item, err := e.provider.GetWorkItem(ctx, e.repo, id)
	if err != nil {
		t.Errorf("cleanup: read work item %s: %v", id, err)
		return
	}
	if !e.ns.ownsItemBody(item.Body, scenario) {
		t.Errorf("cleanup: refusing to retire work item %s: its body lacks this run's footer", id)
		return
	}
	raw, err := rawADOWorkItem(item)
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	states, err := e.provider.adoWorkItemStateCategories(ctx, e.repo, item.Type)
	if err != nil {
		t.Errorf("cleanup: read %s states: %v", item.Type, err)
		return
	}
	target, ok := adoLiveRetireState(states)
	if !ok || strings.EqualFold(stringField(raw.Fields, "System.State"), target) {
		return
	}
	endpoint, err := e.provider.workURL(e.provider.project(e.repo), "workitems", id)
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	patch := []adoPatchOperation{
		{Op: "test", Path: "/rev", Value: raw.Rev},
		{Op: "add", Path: "/fields/System.State", Value: target},
	}
	if err := e.provider.doPatch(ctx, http.MethodPatch, endpoint, patch, nil); err != nil {
		t.Errorf("cleanup: move work item %s to %s: %v", id, target, err)
	}
}
