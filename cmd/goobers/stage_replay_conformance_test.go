package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// Stage replay conformance: the post-then-verify and dedupe class.
//
// A stage that writes and then reads back or dedupes against what it wrote
// must survive a retry, and must do so with the attribution footer every
// daemon stage stores on its writes. Each case below runs one registered
// stage twice, as a stage retry does, with the suite's default attribution
// on (defaultTestStageAttribution), against a fake that stores bodies
// verbatim behind a write recorder. It requires:
//
//   - exit 0 on both runs;
//   - run 1's writes within the case's declared budget;
//   - run 2 to create nothing and to stay within its declared updates;
//   - the end state to hold exactly one Goobers-owned artifact per slot, with
//     a valid attribution marker.
//
// TestStageReplayRoster makes every registered stage either a case here or a
// replayExempt entry with a reason, so a new stage that writes cannot land
// without being classified.

// replayFixture is one stage wired to a verbatim-storing fake.
type replayFixture struct {
	// run invokes the stage once, the way the runner does.
	run func(t *testing.T) (code int, stdout, stderr string)
	// writes records every provider write the stage makes.
	writes *providerWriteCounter
	// owned returns the Goobers-owned artifacts the stage keeps, by slot: a
	// correct stage leaves exactly one body in each.
	owned func(t *testing.T) map[string][]string
}

// stageReplayCase is one stage on one provider. first and replay bound the
// writes of run 1 and run 2 by route (method plus path, numbers folded to
// {n}); a route missing from a budget must not be written on that run.
// creates names the routes that create an artifact: run 2 must make none.
type stageReplayCase struct {
	stage    string
	provider providers.ProviderKind
	setup    func(t *testing.T) replayFixture
	first    map[string]int
	replay   map[string]int
	creates  []string
}

func TestStageReplayConformance(t *testing.T) {
	for _, tc := range stageReplayCases() {
		t.Run(tc.stage+"/"+string(tc.provider), func(t *testing.T) {
			for _, route := range tc.creates {
				if tc.replay[route] != 0 {
					t.Fatalf("case budget lets run 2 create through %s", route)
				}
			}
			f := tc.setup(t)
			f.writes.take()
			for attempt, budget := range []map[string]int{tc.first, tc.replay} {
				code, stdout, stderr := f.run(t)
				if code != 0 {
					t.Fatalf("run %d: code = %d\nstdout: %s\nstderr: %s", attempt+1, code, stdout, stderr)
				}
				assertReplayWritesWithinBudget(t, attempt+1, f.writes.take(), budget)
			}
			owned := f.owned(t)
			if len(owned) == 0 {
				t.Fatal("stage left no Goobers-owned artifact")
			}
			for _, slot := range slices.Sorted(maps.Keys(owned)) {
				bodies := owned[slot]
				if len(bodies) != 1 {
					t.Errorf("%s: %d Goobers-owned artifacts, want exactly one: %q", slot, len(bodies), bodies)
					continue
				}
				if _, ok, err := providers.ParseAttribution(bodies[0]); err != nil || !ok {
					t.Errorf("%s: artifact carries no valid attribution (ok=%v, err=%v): %q", slot, ok, err, bodies[0])
				}
			}
		})
	}
}

// replayLabelRoute folds the label name out of a label-removal route, which
// the write counter leaves in the path.
var replayLabelRoute = regexp.MustCompile(`/labels/.+$`)

func assertReplayWritesWithinBudget(t *testing.T, run int, recorded, budget map[string]int) {
	t.Helper()
	got := map[string]int{}
	for route, count := range recorded {
		got[replayLabelRoute.ReplaceAllString(route, "/labels/{label}")] += count
	}
	t.Logf("run %d writes: %v", run, got)
	for _, route := range slices.Sorted(maps.Keys(got)) {
		if limit, ok := budget[route]; !ok || got[route] > limit {
			t.Errorf("run %d: %d x %s, budget %d (all writes: %v)", run, got[route], route, limit, got)
		}
	}
}

// recordReplayWrite records r when it writes. A GraphQL POST is a write only
// when it carries a mutation: GitHub serves review-thread reads over GraphQL
// too.
func recordReplayWrite(counter *providerWriteCounter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql") && r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(data))
		var request struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(data, &request) == nil && !strings.HasPrefix(strings.TrimSpace(request.Query), "mutation") {
			return
		}
	}
	counter.record(r)
}

// replayCountingClient records every write before sending it.
type replayCountingClient struct {
	counter *providerWriteCounter
	inner   *http.Client
}

func (c replayCountingClient) Do(r *http.Request) (*http.Response, error) {
	recordReplayWrite(c.counter, r)
	return c.inner.Do(r)
}

// recordServerWrites puts a write recorder in front of an existing fake.
func recordServerWrites(t *testing.T, fake http.Handler, counter *providerWriteCounter) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordReplayWrite(counter, r)
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// recordGitHubWrites routes the stage's GitHub provider at server through a
// write recorder. Call it after any helper that installs server itself.
func recordGitHubWrites(t *testing.T, server *fakeGitHubServer) *providerWriteCounter {
	t.Helper()
	counter := &providerWriteCounter{}
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return server.newGitHubProvider(token, append(opts, providers.WithHTTPClient(replayCountingClient{counter: counter, inner: &http.Client{}}))...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })
	return counter
}

func replayStageRun(stage string, args ...string) func(t *testing.T) (int, string, string) {
	return func(t *testing.T) (int, string, string) {
		t.Helper()
		return runArgs(t, append([]string{stage}, args...)...)
	}
}

// ownedGitHubComments returns number's comments that satisfy mine.
func ownedGitHubComments(t *testing.T, server *fakeGitHubServer, number int, mine func(string) bool) []string {
	t.Helper()
	comments, _ := fakeIssueComments(t, server, number)
	var owned []string
	for _, comment := range comments {
		if mine(providers.StripAttribution(comment)) {
			owned = append(owned, comment)
		}
	}
	return owned
}

const (
	replayGitHubCommentCreate = "POST /repos/your-org/your-repo/issues/{n}/comments"
	replayGitHubCommentEdit   = "PATCH /repos/your-org/your-repo/issues/comments/{n}"
	replayGitHubLabelAdd      = "POST /repos/your-org/your-repo/issues/{n}/labels"
	replayGitHubLabelRemove   = "DELETE /repos/your-org/your-repo/issues/{n}/labels/{label}"
	replayGitHubIssueCreate   = "POST /repos/your-org/your-repo/issues"
	replayGitHubIssueEdit     = "PATCH /repos/your-org/your-repo/issues/{n}"
	replayGitHubReviewReply   = "POST /repos/your-org/your-repo/pulls/{n}/comments/{n}/replies"
	replayGitHubGraphQL       = "POST /graphql"
	replayGitHubReviewCreate  = "POST /repos/your-org/your-repo/pulls/{n}/reviews"
	replayGiteaCommentCreate  = "POST /api/v1/repos/your-org/your-repo/issues/{n}/comments"
	replayGiteaCommentEdit    = "PATCH /api/v1/repos/your-org/your-repo/issues/comments/{n}"
)

func stageReplayCases() []stageReplayCase {
	return []stageReplayCase{
		{
			stage: "resolve-review-threads", provider: providers.ProviderGitHub,
			setup:   replayResolveReviewThreadsGitHub,
			first:   map[string]int{replayGitHubReviewReply: 3, replayGitHubGraphQL: 1},
			replay:  map[string]int{},
			creates: []string{replayGitHubReviewReply},
		},
		{
			stage: "resolve-review-threads", provider: providers.ProviderADO,
			setup:   replayResolveReviewThreadsADO,
			first:   map[string]int{replayADOThreadReply: 3, replayADOThreadEdit: 1},
			replay:  map[string]int{},
			creates: []string{replayADOThreadReply},
		},
		{
			stage: "respond-to-findings", provider: providers.ProviderGitHub,
			setup:   replayRespondToFindingsGitHub,
			first:   map[string]int{replayGitHubCommentCreate: 1},
			replay:  map[string]int{replayGitHubCommentEdit: 1},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			stage: "respond-to-findings", provider: providers.ProviderGitea,
			setup:   replayRespondToFindingsGitea,
			first:   map[string]int{replayGiteaCommentCreate: 1},
			replay:  map[string]int{replayGiteaCommentEdit: 1},
			creates: []string{replayGiteaCommentCreate},
		},
		{
			stage: "respond-to-findings", provider: providers.ProviderADO,
			setup:   replayRespondToFindingsADO,
			first:   map[string]int{replayADOThreadCreate: 1},
			replay:  map[string]int{replayADOThreadCommentEdit: 1},
			creates: []string{replayADOThreadCreate},
		},
		{
			// The merge-review status comment is created once and edited in
			// place on a retry. The native review is a vote apply-verdict
			// re-submits on every run; it is never read back, so it is budgeted
			// rather than counted as an artifact.
			stage: "apply-verdict", provider: providers.ProviderGitHub,
			setup: replayApplyVerdictGitHub,
			first: map[string]int{
				replayGitHubCommentCreate: 1, replayGitHubLabelAdd: 1, replayGitHubReviewCreate: 1,
			},
			replay: map[string]int{
				replayGitHubCommentEdit: 1, replayGitHubLabelAdd: 1, replayGitHubReviewCreate: 1,
			},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			stage: "apply-verdict", provider: providers.ProviderGitea,
			setup:   replayMergeReviewStatusGitea,
			first:   map[string]int{replayGiteaCommentCreate: 1},
			replay:  map[string]int{replayGiteaCommentEdit: 1},
			creates: []string{replayGiteaCommentCreate},
		},
		{
			stage: "issue-close-out", provider: providers.ProviderGitHub,
			setup:   replayIssueCloseOutGitHub,
			first:   map[string]int{replayGitHubCommentCreate: 1, replayGitHubLabelAdd: 1},
			replay:  map[string]int{},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			// The second half of the issue-close-out round trip: the PR closed
			// unmerged, so the claim requeues the issue and claims it again.
			stage: "backlog-query", provider: providers.ProviderGitHub,
			setup: replayBacklogQueryRequeueGitHub,
			first: map[string]int{
				replayGitHubCommentCreate: 1, replayGitHubLabelAdd: 1, replayGitHubLabelRemove: 1,
			},
			replay:  map[string]int{},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			// The retry of a feedback attempt that posted its evidence comment
			// and failed to remove the ready label: the fixture runs that
			// attempt, so run 1 here is the retry and creates nothing.
			stage: "backlog-health", provider: providers.ProviderGitHub,
			setup:   replayBacklogHealthFeedbackGitHub,
			first:   map[string]int{replayGitHubLabelRemove: 1},
			replay:  map[string]int{},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			// Decomposition source selection claims through ClaimWorkItem and
			// skips a parent carrying publish-batch's attributed marker.
			stage: "select-source", provider: providers.ProviderGitHub,
			setup:   replaySelectSourceGitHub,
			first:   map[string]int{replayGitHubCommentCreate: 1, replayGitHubLabelAdd: 1},
			replay:  map[string]int{},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			// Run 1 files both children, links them under the parent and to
			// each other, and posts the prepared record, the parent and child
			// notes, the published record and the claim release. Run 2 finds
			// the published record, re-verifies the attributed children, and
			// writes nothing.
			stage: "publish-batch", provider: providers.ProviderGitHub,
			setup: replayPublishBatchGitHub,
			first: map[string]int{
				replayGitHubIssueCreate: 2, replayGitHubCommentCreate: 6, replayGitHubIssueEdit: 2,
				replayGitHubLabelAdd: 3, replayGitHubLabelRemove: 4,
				replayGitHubSubIssueAttach: 2, replayGitHubBlockedByAttach: 1,
			},
			replay: map[string]int{},
			creates: []string{
				replayGitHubIssueCreate, replayGitHubCommentCreate, replayGitHubSubIssueAttach, replayGitHubBlockedByAttach,
			},
		},
		{
			// The sticky demotion record is created once and edited in place:
			// run 2 is a second refusal at the same head, so it accumulates.
			stage: "record-merge-refusal", provider: providers.ProviderGitHub,
			setup:   replayRecordMergeRefusalGitHub,
			first:   map[string]int{replayGitHubCommentCreate: 1},
			replay:  map[string]int{replayGitHubCommentEdit: 1},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			// Run 2 retries after a crash between issue #42's close-out comment
			// and its checkpoint; see replayReconcilePostMergeGitHub.
			stage: "reconcile-post-merge", provider: providers.ProviderGitHub,
			setup: replayReconcilePostMergeGitHub,
			first: map[string]int{
				replayGitHubCommentCreate: 2, replayGitHubBranchDelete: 1,
				replayGitHubIssueEdit: 1, replayGitHubLabelAdd: 1,
			},
			replay:  map[string]int{},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			// The close-out and cost summary are created once and deduped on
			// a retry; the displaced sibling's remediation handoff is found
			// by its marker and edited in place. Run 1 also closes issue 42
			// and labels it done, and labels the sibling needs-remediation.
			stage: "post-merge", provider: providers.ProviderGitHub,
			setup: replayPostMergeGitHub,
			first: map[string]int{
				replayGitHubCommentCreate: 3, replayGitHubLabelAdd: 2, replayGitHubIssueEdit: 1,
			},
			replay:  map[string]int{replayGitHubCommentEdit: 1},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			// update-behind-pr writes nothing of its own; it reads back the
			// attributed merge-review status comment apply-verdict posted,
			// and a substantive finding there must route the behind PR to
			// full remediation on both runs, with no API branch update.
			stage: "update-behind-pr", provider: providers.ProviderGitHub,
			setup:  replayUpdateBehindPRGitHub,
			first:  map[string]int{},
			replay: map[string]int{},
		},
		{
			// The unchanged-digest park edits its sticky remediation-state
			// comment by id; run 2 finds the PR parked by that same comment.
			stage: "gather-pr-context", provider: providers.ProviderGitHub,
			setup: replayGatherPRContextGitHub,
			first: map[string]int{
				replayGitHubLabelAdd: 1, replayGitHubLabelRemove: 1, replayGitHubCommentEdit: 1,
			},
			replay:  map[string]int{},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			// A legacy sibling-overlap handoff is migrated in place once; the
			// retry reads its own attributed, migrated handoff back as current.
			stage: "rebase-pr", provider: providers.ProviderGitHub,
			setup:  replayRebasePRSiblingHandoffGitHub,
			first:  map[string]int{replayGitHubCommentEdit: 1},
			replay: map[string]int{},
		},
		{
			// The sticky remediation-state comment is created once and edited
			// in place on a retry. The retry reads back its own run's write for
			// the same head, so it is idempotent (#6008): no escalation label
			// swap and no second comment.
			stage: "remediation-checkpoint", provider: providers.ProviderGitHub,
			setup:   replayRemediationCheckpointGitHub,
			first:   map[string]int{replayGitHubCommentCreate: 1},
			replay:  map[string]int{replayGitHubCommentEdit: 1},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			// push-remediated writes no comment: it reads back the attributed
			// remediation-state comment for its lease and clears the label.
			stage: "push-remediated", provider: providers.ProviderGitHub,
			setup:   replayPushRemediatedGitHub,
			first:   map[string]int{replayGitHubLabelRemove: 1},
			replay:  map[string]int{replayGitHubLabelRemove: 1},
			creates: []string{replayGitHubCommentCreate},
		},
		{
			stage: "push-remediated", provider: providers.ProviderADO,
			setup:   replayPushRemediatedADO,
			first:   map[string]int{replayADOPullRequestLabelClear: 1},
			replay:  map[string]int{},
			creates: []string{replayADOThreadCreate, replayADOThreadReply},
		},
		{
			stage: "file-issues", provider: providers.ProviderGitHub,
			setup:   replayFileIssuesGitHub,
			first:   map[string]int{replayGitHubIssueCreate: 1},
			replay:  map[string]int{},
			creates: []string{replayGitHubIssueCreate},
		},
	}
}

func replayResolveReviewThreadsGitHub(t *testing.T) replayFixture {
	const runID = "replay-resolve-threads"
	root := initDemo(t)
	seedReviewThreadResolutionRun(t, root, runID, `[
		{"threadId":"PRRT_addressed","disposition":"addressed","detail":"added synchronization"},
		{"threadId":"PRRT_obsolete","disposition":"obsolete","detail":"code was removed"},
		{"threadId":"PRRT_blocked","disposition":"blocked","detail":"needs maintainer input"}
	]`)
	fake, threads := newGitHubReviewThreadsFake(t)
	counter := &providerWriteCounter{}
	server := recordServerWrites(t, fake.Config.Handler, counter)
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = server.URL })...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Chdir(t.TempDir())
	return replayFixture{
		run:    replayStageRun("resolve-review-threads", root),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			owned := map[string][]string{}
			for id, state := range threads {
				owned[id] = nil
				for _, reply := range state.replies {
					owned[id] = append(owned[id], reply["body"].(string))
				}
			}
			return owned
		},
	}
}

const (
	replayADOThreadReply       = "POST /acme/project/_apis/git/repositories/web/pullrequests/{n}/threads/{n}/comments"
	replayADOThreadEdit        = "PATCH /acme/project/_apis/git/repositories/web/pullrequests/{n}/threads/{n}"
	replayADOThreadCreate      = "POST /acme/project/_apis/git/repositories/web/pullrequests/{n}/threads"
	replayADOThreadCommentEdit = "PATCH /acme/project/_apis/git/repositories/web/pullrequests/{n}/threads/{n}/comments/{n}"
)

func replayResolveReviewThreadsADO(t *testing.T) replayFixture {
	const runID = "replay-ado-resolve-threads"
	root, repo := adoReviewThreadsStageFixture(t, runID)
	seedReviewThreadResolutionRunWithComments(t, root, runID, `[
		{"threadId":"77/5","disposition":"addressed","detail":"added synchronization"},
		{"threadId":"77/6","disposition":"obsolete","detail":"code was removed"},
		{"threadId":"77/7","disposition":"blocked","detail":"needs maintainer input"}
	]`, []apiv1.RemediationInlineComment{
		{ID: 1, ThreadID: "77/5", Body: "fix", Path: "a.go", Integrity: apiv1.IntegrityUnapproved},
		{ID: 1, ThreadID: "77/6", Body: "old", Path: "b.go", Integrity: apiv1.IntegrityUnapproved},
		{ID: 1, ThreadID: "77/7", Body: "blocked", Path: "c.go", Integrity: apiv1.IntegrityUnapproved},
	})
	fake := &fakeADOReviewThreads{t: t, threads: map[int]*adoReviewThread{
		5: {status: "active", path: "a.go"},
		6: {status: "active", path: "b.go"},
		7: {status: "pending", path: "c.go"},
	}}
	inner := fake.server(repo, "published-sha")
	t.Cleanup(inner.Close)
	counter := &providerWriteCounter{}
	routeADOStageProvider(t, recordServerWrites(t, inner.Config.Handler, counter).URL)
	t.Chdir(t.TempDir())
	return replayFixture{
		run:    replayStageRun("resolve-review-threads", root),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			owned := map[string][]string{}
			for id, thread := range fake.threads {
				slot := "thread " + strconv.Itoa(id)
				owned[slot] = nil
				for _, reply := range thread.replies {
					owned[slot] = append(owned[slot], reply["content"].(string))
				}
			}
			return owned
		},
	}
}

func replayRemediationVerdict() apiv1.Verdict {
	return apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "validate empty input"}},
	}
}

const replayRemediationResponses = `[{"finding":1,"disposition":"addressed","detail":"Added an explicit empty-input guard."}]`

func replayRespondToFindingsGitHub(t *testing.T) replayFixture {
	root, server, _ := respondToFindingsFixture(t, replayRemediationVerdict(), replayRemediationResponses, true)
	counter := recordGitHubWrites(t, server)
	return replayFixture{
		run:    replayStageRun("respond-to-findings", root),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			return map[string][]string{"response": ownedGitHubComments(t, server, 77, func(body string) bool {
				return strings.HasPrefix(body, remediationResponseMarker("run-942"))
			})}
		},
	}
}

func replayRespondToFindingsGitea(t *testing.T) replayFixture {
	const runID = "replay-gitea-response"
	t.Chdir(t.TempDir())
	fake, server := newFakeGiteaComments(t, "remediation-bot")
	// A comment another account left on the pull request is not Goobers'
	// and must be left alone.
	fake.addComment("reviewer", "Please also cover the empty case.")
	root := initDemo(t)
	configureRemediationGitea(t, root, server.URL)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitea))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "gitea-issues-token")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), remediationResponseArtifactName))
	if _, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	seedRemediationResponseRun(t, root, runID, replayRemediationVerdict(), replayRemediationResponses, true)
	return replayFixture{
		run:    replayStageRun("respond-to-findings", root),
		writes: &fake.writes,
		owned: func(t *testing.T) map[string][]string {
			var owned []string
			for _, body := range fake.bodies() {
				if strings.HasPrefix(body, remediationResponseMarker(runID)) {
					owned = append(owned, body)
				}
			}
			return map[string][]string{"response": owned}
		},
	}
}

func replayRespondToFindingsADO(t *testing.T) replayFixture {
	const runID = "replay-ado-respond"
	root, repo := respondToFindingsADOFixture(t, runID, replayRemediationVerdict(), replayRemediationResponses)
	fake := &fakeADOResponseThreads{t: t}
	inner := fake.server(repo)
	t.Cleanup(inner.Close)
	counter := &providerWriteCounter{}
	routeADOResponseProvider(t, recordServerWrites(t, inner.Config.Handler, counter).URL)
	return replayFixture{
		run:    replayStageRun("respond-to-findings", root),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			return map[string][]string{"response": fake.ownResponses(runID)}
		},
	}
}

func replayApplyVerdictGitHub(t *testing.T) replayFixture {
	const runID = "replay-apply-verdict"
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(10, "Selected PR")
	server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10head", "shamainbase",
		false, nil, []fakePRFile{{path: "internal/runner/run.go", status: "modified", additions: 5, deletions: 1}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges, Rationale: "the parser drops empty input",
		HeadSHA: "sha10head", BaseSHA: "shamainbase",
		Findings: []apiv1.Finding{{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "validate empty input"}},
	})
	counter := recordGitHubWrites(t, server)
	t.Chdir(t.TempDir())
	return replayFixture{
		run:    replayStageRun("apply-verdict", root),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			return map[string][]string{"merge-review status": ownedGitHubComments(t, server, 10, isMergeReviewStatusComment)}
		},
	}
}

// replayMergeReviewStatusGitea replays apply-verdict's merge-review status
// comment on Gitea through the stage's own provider construction.
func replayMergeReviewStatusGitea(t *testing.T) replayFixture {
	fake, server := newFakeGiteaComments(t, "goobers")
	fake.addComment("reviewer", "Looks close; one more pass please.")
	root := initDemo(t)
	configureRemediationGitea(t, root, server.URL)
	// The provider records its pull request mutations into the stage's
	// working directory, as it does in a stage worktree.
	t.Chdir(t.TempDir())
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "gitea-pr-token")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "your-org", Name: "your-repo"}
	body := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "validate empty input"})
	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			provider, err := newApplyVerdictProviderForRepo(root, repo)
			if err != nil {
				return 1, "", err.Error()
			}
			status, ok := provider.(remediationProvider)
			if !ok {
				return 1, "", "apply-verdict provider has no comment surface"
			}
			if err := reconcileMergeReviewStatusComment(context.Background(), status, repo, 77, body); err != nil {
				return 1, "", err.Error()
			}
			return 0, "", ""
		},
		writes: &fake.writes,
		owned: func(t *testing.T) map[string][]string {
			var owned []string
			for _, comment := range fake.bodies() {
				if isMergeReviewStatusComment(providers.StripAttribution(comment)) {
					owned = append(owned, comment)
				}
			}
			return map[string][]string{"merge-review status": owned}
		},
	}
}

// replayIssueCloseOutFixture claims issue 7 for the close-out run and opens
// its implementation PR, as the implementation workflow leaves them.
func replayIssueCloseOutFixture(t *testing.T) (string, *fakeGitHubServer) {
	t.Helper()
	const closeOutRun = "run-1"
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, _, err := ledger.Claim("7", closeOutRun, "implementation", time.Hour); err != nil {
		t.Fatalf("seed claim ledger: %v", err)
	}
	server.mu.Lock()
	server.prs[1] = &fakePR{number: 1, title: "Implementation", head: providers.BranchName("implementation", closeOutRun), base: "main", state: "open"}
	server.nextPR = 2
	server.mu.Unlock()
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", closeOutRun)
	t.Setenv("GOOBERS_INPUT_STATUS", "in-review")
	t.Chdir(t.TempDir())
	return root, server
}

func isImplementationInReviewBreadcrumb(body string) bool {
	return strings.HasPrefix(body, "Implementation complete: ") && strings.HasSuffix(body, " is open for merge-review.")
}

func replayIssueCloseOutGitHub(t *testing.T) replayFixture {
	root, server := replayIssueCloseOutFixture(t)
	counter := recordGitHubWrites(t, server)
	return replayFixture{
		run:    replayStageRun("issue-close-out", root),
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			return map[string][]string{"merge-review breadcrumb": ownedGitHubComments(t, server, 7, isImplementationInReviewBreadcrumb)}
		},
	}
}

// replayBacklogQueryRequeueGitHub is the second half of the issue-close-out
// round trip: the implementation PR closed unmerged after the attributed
// close-out, so backlog-query must requeue and claim the issue again.
func replayBacklogQueryRequeueGitHub(t *testing.T) replayFixture {
	root, server := replayIssueCloseOutFixture(t)
	if code, stdout, stderr := runArgs(t, "issue-close-out", root); code != 0 {
		t.Fatalf("issue-close-out: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	server.setPRClosed(1)
	const claimRun = "run-2"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", claimRun)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "pr-token")
	t.Setenv("GOOBERS_INPUT_STATUS", "")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", inReviewStatusLabel)
	counter := recordGitHubWrites(t, server)
	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			t.Chdir(t.TempDir())
			code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
			if code == 0 && !strings.Contains(stdout, "claimed 7") {
				return 1, stdout, "issue 7 was not requeued and claimed: " + stderr
			}
			return code, stdout, stderr
		},
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			return map[string][]string{"claim": ownedGitHubComments(t, server, 7, func(body string) bool {
				return claimRunIDOf(body) == claimRun
			})}
		},
	}
}

// claimRunIDOf returns the run a claim breadcrumb names, or "".
func claimRunIDOf(body string) string {
	first, _, _ := strings.Cut(body, "\n")
	run, ok := strings.CutPrefix(strings.TrimSpace(first), "goobers-claim: run=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(run)
}

func replayFileIssuesGitHub(t *testing.T) replayFixture {
	f := newFileIssuesFixture(t)
	f.writeArtifact(lowRisk("replay"))
	counter := recordGitHubWrites(t, f.server)
	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			return f.run()
		},
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			f.server.mu.Lock()
			defer f.server.mu.Unlock()
			var owned []string
			for _, number := range sortedIntKeys(f.server.issues) {
				owned = append(owned, f.server.issues[number].body)
			}
			return map[string][]string{"filed issue": owned}
		},
	}
}

// replayExempt classifies every registered stage that is not a replay case,
// with the reason it is not one. A reason starts with its class:
//
//   - "read-only:" the stage makes no provider write;
//   - "no read-back:" it writes, but never reads back or dedupes against text
//     it wrote, so the attribution footer cannot change what it decides.
//
// There is no deferral class: a stage that reads back text Goobers wrote,
// its own or another stage's, is a replay case.
var replayExempt = map[string]string{
	"recovery-resume":        "read-only: re-attaches a recovered run's branch; local state only",
	"backlog-dedupe":         "read-only: scores candidate duplicates into its result file (footer-free similarity: backlogdedupeattribution_test.go)",
	"backlog-assignment":     "no read-back: assigns items without a comment",
	"validate-plan":          "read-only: validates the plan artifact",
	"reconcile-branches":     "no read-back: reconciles git branches, no provider text",
	"push-branch":            "no read-back: pushes a git branch",
	"preflight-repo-write":   "no read-back: probes push permission with a scratch ref",
	"check-fail-first":       "read-only: runs the project's tests locally",
	"open-pr":                "no read-back: finds an existing pull request by head branch, not by text it wrote",
	"report-pr-status":       "no read-back: publishes a native pull request status",
	"gate-removal-guard":     "no read-back: labels and comments on a guard hit without reading its comment back",
	"set-milestone":          "no read-back: sets a milestone field",
	"merge-pr":               "no read-back: merges; refusals are recorded by record-merge-refusal",
	"merge-queue-poll":       "no read-back: labels and comments on a queue failure",
	"security-alerts-query":  "read-only: lists security alerts",
	"telemetry-query":        "read-only: queries local telemetry",
	"docs-churn":             "read-only: analyses local git history",
	"ios-simulator-test":     "read-only: runs simulator tests locally",
	"pr-select":              "read-only: selects a pull request from provider state",
	"cancel-pending-ci":      "no read-back: cancels pending CI runs",
	"check-issue-staleness":  "no read-back: comments and labels when the issue spec changed",
	"gather-sibling-context": "read-only: gathers sibling pull request context and the cached verdict",
	gatherContextID:          "read-only: gathers implementation context",
	"elect-lander":           "read-only: elects a lander from the verdict and sibling state",
	"pr-claim":               "read-only: polls the pull request and keeps the local claim",
	"gather-review-threads":  "read-only: gathers review threads",
	"gather-issue-context":   "read-only: gathers issue context",
	"pr-comment-watch":       "no read-back: labels pull requests; its own comments are recognised by author, not text",
	"gather-ci-failures":     "read-only: gathers CI failure logs",
	"mcp-io":                 "read-only: serves the stage MCP bridge",
}

// replayExemptClasses are the reason prefixes a replayExempt entry may use.
var replayExemptClasses = []string{"read-only: ", "no read-back: "}

// TestStageReplayRoster requires every registered stage command to be a
// replay case or a classified replayExempt entry, and neither list to name a
// stage the CLI registry does not have.
func TestStageReplayRoster(t *testing.T) {
	tabled := map[string]bool{}
	for _, tc := range stageReplayCases() {
		tabled[tc.stage] = true
	}
	registered := map[string]bool{}
	var walk func([]cliCommand)
	walk = func(commands []cliCommand) {
		for _, command := range commands {
			if command.tier == cliTierStage {
				registered[command.names[0]] = true
			}
			walk(command.subcommands)
		}
	}
	walk(cliCommands)
	if len(registered) == 0 {
		t.Fatal("the CLI registry lists no stage commands")
	}
	for _, stage := range slices.Sorted(maps.Keys(registered)) {
		_, exempt := replayExempt[stage]
		switch {
		case tabled[stage] && exempt:
			t.Errorf("stage %q is both a replay case and replayExempt; drop the exemption", stage)
		case !tabled[stage] && !exempt:
			t.Errorf("stage %q is neither a replay case nor classified in replayExempt: add a case to stageReplayCases, "+
				"or an exemption saying why it is read-only or never reads back text it wrote", stage)
		}
	}
	for _, stage := range slices.Sorted(maps.Keys(replayExempt)) {
		if !registered[stage] {
			t.Errorf("replayExempt names %q, which is not a registered stage command", stage)
		}
		reason := replayExempt[stage]
		if !slices.ContainsFunc(replayExemptClasses, func(class string) bool {
			return strings.HasPrefix(reason, class) && strings.TrimSpace(strings.TrimPrefix(reason, class)) != ""
		}) {
			t.Errorf("replayExempt[%q] = %q; a reason starts with one of %q and says why", stage, reason, replayExemptClasses)
		}
	}
	for _, stage := range slices.Sorted(maps.Keys(tabled)) {
		if !registered[stage] {
			t.Errorf("replay case names %q, which is not a registered stage command", stage)
		}
	}
}
