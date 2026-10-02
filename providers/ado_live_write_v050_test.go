//go:build liveadowrite

// Live write-leg scenarios for the Azure DevOps features of the v0.5 cycle:
// the capped PR description keeping its closing references (#6175), native
// PR-to-work-item linking (#6115), the comment read pr-comment-watch builds
// its watermarks on (#6130/#6135), policy-evaluation classification
// including "Require a merge strategy" (#6106), and native CI failure
// evidence from a build's timeline and logs (#5652/#6137). They follow the
// leg's rules (ado_live_write_test.go): run-namespaced branches and work
// items, find-or-create, own cleanup, nothing shared deleted.
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	// adoLiveCIFailureDefinitionEnv names the build definition
	// `go run ./test/adolive provision -ci-pipeline` creates (repository
	// variable ADO_LIVE_CI_FAILURE_PIPELINE).
	adoLiveCIFailureDefinitionEnv = "GOOBERS_ADO_LIVE_CI_FAILURE_PIPELINE"
	// adoLiveCIFailureYAML must match test/adolive's ciPipelineYAML: the
	// definition reads it from the branch it is queued on.
	adoLiveCIFailureYAML = ".goobers-live/ci-failure.yml"
	adoLiveCIFailureStep = "goobers-live failing step"
	// adoLiveBuildTimeout bounds the wait for the hosted agent to pick up and
	// finish the failing build; it is a poll budget, never an assertion.
	adoLiveBuildTimeout = 15 * time.Minute
	// adoLivePollInterval paces the bounded polls that absorb ADO's
	// eventually consistent policy evaluations and projections.
	adoLivePollInterval = 5 * time.Second
	adoLivePollAttempts = 24
)

// TestLiveADOWriteNativePullRequest opens one run-namespaced pull request
// whose description exceeds ADO's cap and closes the run's own work item, then
// checks what the provider reads back from it.
func TestLiveADOWriteNativePullRequest(t *testing.T) {
	env := adoLiveWriteSetup(t)
	ctx := adoLiveContext(t)
	const scenario = "native"
	branch := env.ns.branch(scenario)
	t.Cleanup(func() { env.cleanupPullRequests(t, branch) })
	env.ensureBranch(ctx, t, branch)

	item, err := env.provider.CreateWorkItem(ctx, CreateWorkItemRequest{
		Repository: env.repo,
		Type:       strings.TrimSpace(os.Getenv("GOOBERS_ADO_LIVE_WORK_ITEM_TYPE")),
		Title:      "[goobers-live] linked work item " + env.ns.runID + " (safe to remove)",
		Body:       "Automated Azure DevOps provider write leg work item. Retired by its own cleanup.",
		Labels:     []string{adoLiveTag},
		RunID:      env.ns.itemRunID(scenario),
	})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	t.Cleanup(func() { env.retireWorkItem(t, item.ID, scenario) })

	closing := []string{"Fixes #" + item.ID, "Closes " + item.URL}
	req := PullRequestRequest{
		Repository: env.repo,
		Title:      "[goobers-live] native features " + env.ns.runID + " (safe to abandon)",
		Body: strings.Repeat("goobers live write leg oversized description line.\n", 120) +
			"\n" + strings.Join(closing, "\n") + "\n",
		Head: branch,
		Base: env.base,
		// Not a draft: the policy classification scenario needs ADO to
		// evaluate the base branch's policies, which it skipped on the
		// leg's draft pull request. Nothing ever completes it.
		Draft: false,
		RunID: adoLiveTag + "-" + env.ns.runID + "-" + scenario,
	}
	if utf8.RuneCountInString(req.Body) <= adoMaxPRDescriptionChars {
		t.Fatalf("oversized body is only %d characters", utf8.RuneCountInString(req.Body))
	}
	pr, err := env.provider.OpenPullRequest(ctx, req)
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	t.Logf("pull request %s: %s; work item %s", pr.ID, pr.URL, item.ID)

	t.Run("capped description keeps closing references", func(t *testing.T) {
		env.checkCappedClosingReferences(ctx, t, pr.ID, req, closing)
	})
	t.Run("native work-item link", func(t *testing.T) { env.checkNativeLink(ctx, t, pr.ID, item.ID) })
	t.Run("feedback comments", func(t *testing.T) { env.checkFeedbackComments(ctx, t, pr.ID) })
	t.Run("policy classification", func(t *testing.T) { env.checkPolicyClassification(ctx, t, pr.ID) })
}

// checkCappedClosingReferences reads the stored description back: ADO kept
// it within its cap, and the closing references that sat at the end of the
// oversized body survived the trim (#6175), as did the run-id footer.
func (e adoLiveWriteEnv) checkCappedClosingReferences(ctx context.Context, t *testing.T, pullID string, req PullRequestRequest, closing []string) {
	got, err := e.provider.PollPullRequest(ctx, PullRequestPollRequest{Repository: e.repo, PullID: pullID})
	if err != nil {
		t.Fatalf("PollPullRequest: %v", err)
	}
	if n := utf8.RuneCountInString(got.Body); n > adoMaxPRDescriptionChars {
		t.Fatalf("stored description is %d characters, over the %d cap", n, adoMaxPRDescriptionChars)
	}
	for _, want := range append(slices.Clone(closing), "description truncated", runFooter(req.RunID)) {
		if !strings.Contains(got.Body, want) {
			t.Errorf("stored description lacks %q; its tail is %q", want, adoLiveTail(got.Body, 400))
		}
	}
}

// checkNativeLink links the pull request to the run's work item through ADO's
// native artifact relation (#6115), twice, and reads both projections back:
// exactly one relation on the work item, and the work item on the pull
// request's linked work items.
func (e adoLiveWriteEnv) checkNativeLink(ctx context.Context, t *testing.T, pullID, workItemID string) {
	for range 2 {
		if err := e.provider.LinkPullRequestToWorkItem(ctx, e.repo, e.repo, workItemID, pullID); err != nil {
			t.Fatalf("LinkPullRequestToWorkItem: %v", err)
		}
	}
	raw, err := e.provider.getRawWorkItem(ctx, e.repo, workItemID)
	if err != nil {
		t.Fatalf("read work item %s: %v", workItemID, err)
	}
	links := 0
	for _, relation := range raw.Relations {
		if relation.Rel == "ArtifactLink" && strings.HasPrefix(relation.URL, "vstfs:///Git/PullRequestId/") &&
			strings.HasSuffix(strings.ToLower(relation.URL), "%2f"+pullID) {
			links++
		}
	}
	if links != 1 {
		t.Fatalf("work item %s carries %d pull request %s artifact link(s), want exactly 1: %+v", workItemID, links, pullID, raw.Relations)
	}
	endpoint, err := e.provider.repoURL(e.repo, "pullrequests", pullID, "workitems")
	if err != nil {
		t.Fatal(err)
	}
	adoLivePoll(ctx, t, "pull request "+pullID+" lists work item "+workItemID, func() (bool, string, error) {
		var out struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
		}
		if err := e.provider.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
			return false, "", err
		}
		ids := make([]string, 0, len(out.Value))
		for _, ref := range out.Value {
			ids = append(ids, ref.ID)
		}
		return slices.Contains(ids, workItemID), fmt.Sprintf("linked work items %v", ids), nil
	})
}

// checkFeedbackComments posts one plain and one attributed comment and reads
// them through ListPullRequestFeedbackComments, the read pr-comment-watch
// classifies (#6130/#6135). Its classifier keys on two provider facts this
// pins live: a comment's AuthorID is the credential's own identity id
// (dedicated mode makes that Goobers'; shared mode leaves an unmarked one
// human), and a body round-trips intact, so the attribution marker on the
// attributed comment parses and the plain one carries none.
func (e adoLiveWriteEnv) checkFeedbackComments(ctx context.Context, t *testing.T, pullID string) {
	self, err := e.provider.AuthenticatedIdentity(ctx)
	if err != nil {
		t.Fatalf("AuthenticatedIdentity: %v", err)
	}
	plain, err := e.provider.PostPullRequestThreadComment(ctx, e.repo, pullID, "goobers-live plain comment "+e.ns.runID)
	if err != nil {
		t.Fatalf("PostPullRequestThreadComment (plain): %v", err)
	}
	attributing := NewADOProvider(e.provider.Organization, e.repo.Project, os.Getenv("GOOBERS_ADO_LIVE_TOKEN"))
	attributing.SetAttribution(Attribution{
		Instance: "goobers-live", Gaggle: "goobers-live", Workflow: "live-write",
		Task: "feedback-comments", Goober: "live-write", Run: e.ns.runID,
	})
	marked, err := attributing.PostPullRequestThreadComment(ctx, e.repo, pullID, "goobers-live attributed comment "+e.ns.runID)
	if err != nil {
		t.Fatalf("PostPullRequestThreadComment (attributed): %v", err)
	}

	comments, err := e.provider.ListPullRequestFeedbackComments(ctx, e.repo, pullID)
	if err != nil {
		t.Fatalf("ListPullRequestFeedbackComments: %v", err)
	}
	for i, comment := range comments {
		if comment.AuthorID == "" || comment.CreatedAt == nil {
			t.Fatalf("feedback comment %s has no author id or publish time: %+v", comment.ID, comment)
		}
		if i > 0 && comment.CreatedAt.Before(*comments[i-1].CreatedAt) {
			t.Fatalf("feedback comments are not in publish order at %s", comment.ID)
		}
	}
	for _, posted := range []struct {
		comment    Comment
		attributed bool
	}{{plain, false}, {marked, true}} {
		i := slices.IndexFunc(comments, func(c Comment) bool { return c.ID == posted.comment.ID })
		if i < 0 {
			t.Fatalf("feedback comments %v lack posted comment %s", adoLiveCommentIDs(comments), posted.comment.ID)
		}
		got := comments[i]
		if !strings.EqualFold(got.AuthorID, self.ID) {
			t.Errorf("comment %s author id %q, want the credential's identity %q", got.ID, got.AuthorID, self.ID)
		}
		attribution, found, err := ParseAttribution(got.Body)
		switch {
		case err != nil:
			t.Errorf("comment %s attribution does not parse: %v", got.ID, err)
		case found != posted.attributed:
			t.Errorf("comment %s attribution found = %v, want %v; body %q", got.ID, found, posted.attributed, got.Body)
		case found && (!attribution.Goobers || attribution.Run != e.ns.runID):
			t.Errorf("comment %s attribution = %+v, want Goobers run %s", got.ID, attribution, e.ns.runID)
		}
	}
}

// checkPolicyClassification reads the policy evaluations the provisioned
// base-branch policies produce on the pull request (#6106). Each is a
// well-known type the provider classifies (the type ids are constants in
// ado_policy.go; a renamed or re-keyed policy type surfaces here first). With
// a passing status published, the CI state is passing: the unmet reviewer
// policy is a wait on a human, and "Require a merge strategy" never gates CI
// or reads as a CI failure.
func (e adoLiveWriteEnv) checkPolicyClassification(ctx context.Context, t *testing.T, pullID string) {
	if _, err := e.provider.PublishPullRequestStatus(ctx, PullRequestStatusRequest{
		Repository: e.repo, PullID: pullID, Genre: adoLiveStatusGenre, Name: adoLiveStatusName,
		State: CheckStatePassing, Description: "goobers live write leg " + e.ns.runID,
	}); err != nil {
		t.Fatalf("PublishPullRequestStatus: %v", err)
	}
	var polled PullRequestPollResult
	// With no blocking evaluation at all the state also reads passing, so
	// wait for the reviewer evaluation to exist, not just for "passing".
	adoLivePoll(ctx, t, "the reviewer and status policy evaluations", func() (bool, string, error) {
		var err error
		polled, err = e.provider.PollPullRequest(ctx, PullRequestPollRequest{Repository: e.repo, PullID: pullID})
		evaluated := slices.ContainsFunc(polled.Checks, func(c CheckDetail) bool { return c.AwaitingHuman })
		return err == nil && evaluated && polled.CheckState == CheckStatePassing,
			fmt.Sprintf("check state %q, checks %+v", polled.CheckState, polled.Checks), err
	})

	detail, err := e.provider.getPullRequestDetail(ctx, e.repo, pullID)
	if err != nil {
		t.Fatalf("read pull request %s: %v", pullID, err)
	}
	evals, err := e.provider.pullRequestEvaluations(ctx, e.repo, pullID, detail)
	if err != nil {
		t.Fatalf("read policy evaluations: %v", err)
	}
	kinds := map[adoPolicyKind]adoPolicyEvaluation{}
	for _, ev := range evals {
		kind := adoPolicyKindOf(ev.Configuration.Type.ID)
		t.Logf("policy %q (type %s): %s, blocking %v", ev.Configuration.Type.DisplayName, ev.Configuration.Type.ID, ev.Status, ev.Configuration.IsBlocking)
		if kind == adoPolicyOther {
			t.Errorf("policy %q type id %s is not a type the provider classifies", ev.Configuration.Type.DisplayName, ev.Configuration.Type.ID)
		}
		kinds[kind] = ev
	}
	for kind, name := range map[adoPolicyKind]string{adoPolicyReviewer: "minimum reviewers", adoPolicyCI: "status"} {
		if _, ok := kinds[kind]; !ok {
			t.Errorf("no %s policy evaluation on pull request %s: provision the scratch repository with `go run ./test/adolive provision -apply`", name, pullID)
		}
	}
	if _, ok := kinds[adoPolicyMergeStrategy]; !ok {
		// Added to the provisioner after the scratch repository was first
		// provisioned: absent until it is re-run, which the leg reports
		// rather than fails on.
		t.Logf("NOTICE: no \"Require a merge strategy\" evaluation on pull request %s; re-run `go run ./test/adolive provision -apply` to cover its classification", pullID)
	}

	reviewer := slices.IndexFunc(polled.Checks, func(c CheckDetail) bool { return c.AwaitingHuman })
	if reviewer < 0 || polled.Checks[reviewer].State != CheckStatePending {
		t.Errorf("checks %+v: want the unmet reviewer policy pending and awaiting a human", polled.Checks)
	}
	if ev, ok := kinds[adoPolicyMergeStrategy]; ok {
		for _, check := range polled.Checks {
			if check.Name == adoPolicyName(ev) && (check.State == CheckStateFailing || check.AwaitingHuman) {
				t.Errorf("merge strategy check %+v: it must neither fail CI nor wait on a human", check)
			}
		}
	}
	failures, err := e.provider.PullRequestCIFailures(ctx, e.repo, pullID)
	if err != nil {
		t.Fatalf("PullRequestCIFailures: %v", err)
	}
	if len(failures.Failures) != 0 {
		t.Errorf("CI failures %+v: reviewer and merge-strategy policies are never CI failures", failures.Failures)
	}
}

// TestLiveADOWriteCIFailureEvidence queues the provisioned failing build on a
// run-namespaced branch, points the required goobers-live status at it, and
// reads the native failure evidence the provider collects from the build's
// timeline and log (#5652/#6137).
//
// It skips until the scratch project has the build definition: run
// `go run ./test/adolive provision -ci-pipeline -apply` (it needs Azure
// Pipelines hosted parallelism in the organization and Build Read & execute
// on the PAT) and set the printed ADO_LIVE_CI_FAILURE_PIPELINE repository
// variable.
func TestLiveADOWriteCIFailureEvidence(t *testing.T) {
	env := adoLiveWriteSetup(t)
	definition := strings.TrimSpace(os.Getenv(adoLiveCIFailureDefinitionEnv))
	if definition == "" {
		t.Skipf("%s is unset: provision the failing build definition with `go run ./test/adolive provision -ci-pipeline -apply` and set ADO_LIVE_CI_FAILURE_PIPELINE", adoLiveCIFailureDefinitionEnv)
	}
	if _, err := strconv.Atoi(definition); err != nil {
		t.Fatalf("%s = %q, want a build definition id", adoLiveCIFailureDefinitionEnv, definition)
	}
	ctx, cancel := context.WithTimeout(context.Background(), adoLiveBuildTimeout+adoLiveCallTimeout)
	t.Cleanup(cancel)
	const scenario = "ci"
	branch := env.ns.branch(scenario)
	t.Cleanup(func() { env.cleanupPullRequests(t, branch) })
	env.ensureBranch(ctx, t, branch)
	marker := "goobers-live deterministic CI failure " + env.ns.runID
	env.ensureCIFailureYAML(ctx, t, branch, marker)

	pr, err := env.provider.OpenPullRequest(ctx, PullRequestRequest{
		Repository: env.repo,
		Title:      "[goobers-live] CI failure evidence " + env.ns.runID + " (safe to abandon)",
		Body:       "Automated Azure DevOps provider write leg. Never merged; abandoned by its own cleanup.",
		Head:       branch, Base: env.base, Draft: true,
		RunID: adoLiveTag + "-" + env.ns.runID + "-" + scenario,
	})
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	build := env.failedBuild(ctx, t, definition, branch)
	buildURL := env.provider.buildResultsURL(env.provider.project(env.repo), strconv.Itoa(build.ID))
	t.Logf("pull request %s, build %d: %s", pr.ID, build.ID, buildURL)
	if _, err := env.provider.PublishPullRequestStatus(ctx, PullRequestStatusRequest{
		Repository: env.repo, PullID: pr.ID, Genre: adoLiveStatusGenre, Name: adoLiveStatusName,
		State: CheckStateFailing, Description: "goobers live CI failure " + env.ns.runID, TargetURL: buildURL,
	}); err != nil {
		t.Fatalf("PublishPullRequestStatus: %v", err)
	}

	var failure CIFailureDetail
	adoLivePoll(ctx, t, "the rejected status policy to report CI failure evidence", func() (bool, string, error) {
		got, err := env.provider.PullRequestCIFailures(ctx, env.repo, pr.ID)
		if err != nil || len(got.Failures) == 0 {
			return false, fmt.Sprintf("failures %+v", got.Failures), err
		}
		failure = got.Failures[0]
		return true, "", nil
	})
	t.Logf("evidence %s: %s", failure.Evidence, failure.Summary)
	if failure.Evidence != CIEvidenceComplete {
		t.Errorf("evidence = %q (%s), want complete", failure.Evidence, failure.Summary)
	}
	var issue, excerpt bool
	excerptBytes := 0
	for _, a := range failure.Annotations {
		if len(a.Message) > adoLogChunkBytes {
			t.Errorf("annotation %q is %d bytes, over the %d chunk bound", a.Title, len(a.Message), adoLogChunkBytes)
		}
		switch {
		case strings.HasPrefix(a.Title, "log excerpt: ") && strings.Contains(a.Title, adoLiveCIFailureStep):
			excerptBytes += len(a.Message)
			excerpt = excerpt || strings.Contains(a.Message, marker)
		case strings.Contains(a.Title, adoLiveCIFailureStep) && a.Level == "error":
			issue = true
		}
	}
	if !issue {
		t.Errorf("no timeline error issue for the failed step %q in %+v", adoLiveCIFailureStep, failure.Annotations)
	}
	if !excerpt {
		t.Errorf("no log excerpt of step %q carrying %q in %+v", adoLiveCIFailureStep, marker, failure.Annotations)
	}
	if limit := defaultADOCIEvidenceBounds.ExcerptBytes + adoLogChunkBytes; excerptBytes > limit {
		t.Errorf("log excerpt is %d bytes, over the bound", excerptBytes)
	}
}

// ensureCIFailureYAML commits the pipeline the provisioned definition reads:
// one step that prints marker and exits non-zero. Only this run's branch
// carries it, and triggers are off, so nothing runs it but this test.
func (e adoLiveWriteEnv) ensureCIFailureYAML(ctx context.Context, t *testing.T, branch, marker string) {
	t.Helper()
	tip, found, err := e.provider.lookupBranchSHA(ctx, e.repo, branch)
	if err != nil || !found {
		t.Fatalf("lookup %s: found %v, %v", branch, found, err)
	}
	yaml := strings.Join([]string{
		"# goobers-live: fails on purpose for the ADO live write leg (#5652). Safe to delete.",
		"trigger: none",
		"pr: none",
		"pool:",
		"  vmImage: ubuntu-latest",
		"steps:",
		"- script: |",
		"    echo \"" + marker + "\"",
		"    exit 3",
		"  displayName: " + adoLiveCIFailureStep,
		"",
	}, "\n")
	if _, err := e.provider.Commit(ctx, CommitRequest{
		Repository: e.repo, Branch: branch, BaseSHA: tip,
		Message: "goobers-live: failing CI pipeline for " + branch,
		Files:   []CommitFile{{Path: adoLiveCIFailureYAML, Content: yaml}},
	}); err != nil {
		t.Fatalf("Commit %s on %s: %v", adoLiveCIFailureYAML, branch, err)
	}
}

// failedBuild finds this branch's build of definition, or queues one, and
// waits within adoLiveBuildTimeout for it to complete. It must fail.
func (e adoLiveWriteEnv) failedBuild(ctx context.Context, t *testing.T, definition, branch string) adoBuild {
	t.Helper()
	project := e.provider.project(e.repo)
	ref := "refs/heads/" + branch
	list, err := e.provider.buildURL(project, url.Values{
		"definitions": []string{definition}, "branchName": []string{ref},
		"queryOrder": []string{"queueTimeDescending"}, "$top": []string{"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var existing adoBuildsResponse
	if err := e.provider.do(ctx, http.MethodGet, list, nil, &existing); err != nil {
		t.Fatalf("list builds: %v", err)
	}
	var build adoBuild
	if len(existing.Value) > 0 {
		build = existing.Value[0]
	} else {
		queue, err := e.provider.buildURL(project, nil)
		if err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"definition": map[string]any{"id": json.Number(definition)}, "sourceBranch": ref}
		if err := e.provider.do(ctx, http.MethodPost, queue, body, &build); err != nil {
			t.Fatalf("queue build of definition %s on %s: %v", definition, branch, err)
		}
	}
	endpoint, err := e.provider.buildURL(project, nil, strconv.Itoa(build.ID))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, adoLiveBuildTimeout)
	defer cancel()
	for !strings.EqualFold(build.Status, "completed") {
		select {
		case <-waitCtx.Done():
			t.Fatalf("build %d is still %q: is a hosted agent available to the project (Azure Pipelines parallelism)?", build.ID, build.Status)
		case <-time.After(10 * time.Second):
		}
		if err := e.provider.do(ctx, http.MethodGet, endpoint, nil, &build); err != nil {
			t.Fatalf("read build %d: %v", build.ID, err)
		}
	}
	if !strings.EqualFold(build.Result, "failed") {
		t.Fatalf("build %d result = %q, want failed: the pipeline's failing step did not run", build.ID, build.Result)
	}
	return build
}

// adoLivePoll retries check until it reports done, within adoLivePollAttempts.
// It tolerates ADO's eventual consistency; it never asserts how long that took.
func adoLivePoll(ctx context.Context, t *testing.T, what string, check func() (bool, string, error)) {
	t.Helper()
	var last string
	var lastErr error
	for range adoLivePollAttempts {
		done, state, err := check()
		if done && err == nil {
			return
		}
		last, lastErr = state, err
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v (last: %s, %v)", what, ctx.Err(), last, lastErr)
		case <-time.After(adoLivePollInterval):
		}
	}
	t.Fatalf("waiting for %s: not reached after %d polls (last: %s, %v)", what, adoLivePollAttempts, last, lastErr)
}

func adoLiveTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func adoLiveCommentIDs(comments []Comment) []string {
	ids := make([]string, 0, len(comments))
	for _, c := range comments {
		ids = append(ids, c.ID)
	}
	return ids
}
