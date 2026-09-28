package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// publish-batch replay: the decomposition publisher adopts its children by an
// idempotency marker in their bodies, adopts its marker comments by a marker
// line, and on a retry reads its published record back from the parent and
// verifies every child body against the plan. Every one of those bodies is
// stored with the daemon's attribution footer, so a verification that
// compares the stored body without StripAttribution reports a false
// PublicationConflict and parks the parent.

const (
	replayPublishBatchParent = 419
	replayPublishBatchRunID  = "replay-publish-batch"

	replayGitHubSubIssueAttach   = "POST /repos/your-org/your-repo/issues/{n}/sub_issues"
	replayGitHubBlockedByAttach  = "POST /repos/your-org/your-repo/issues/{n}/dependencies/blocked_by"
	replayPublishBatchParentBody = "Split this over-scoped issue before implementing it."
)

// withGitHubIssueGraphWrites serves GitHub's native sub-issue and blocked-by
// writes over server's issue state, which the shared fake reads (GET
// sub_issues, GET dependencies/blocked_by) but does not accept. As GitHub
// does, both take the other issue's database id, which the fake serves as
// its number; a repeated link is refused with 422.
func withGitHubIssueGraphWrites(server *fakeGitHubServer, inner http.Handler) http.Handler {
	prefix := "/repos/" + server.owner + "/" + server.repo + "/issues/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, prefix)
		parts := strings.Split(rest, "/")
		subIssue := len(parts) == 2 && parts[1] == "sub_issues"
		blockedBy := len(parts) == 3 && parts[1] == "dependencies" && parts[2] == "blocked_by"
		if !ok || r.Method != http.MethodPost || (!subIssue && !blockedBy) {
			inner.ServeHTTP(w, r)
			return
		}
		number, err := strconv.Atoi(parts[0])
		if err != nil {
			http.Error(w, "bad issue number", http.StatusBadRequest)
			return
		}
		var body struct {
			SubIssueID int64 `json:"sub_issue_id"`
			IssueID    int64 `json:"issue_id"`
		}
		decodeFakeJSON(r, &body)
		linked := int(body.SubIssueID)
		if blockedBy {
			linked = int(body.IssueID)
		}
		server.mu.Lock()
		defer server.mu.Unlock()
		issue, found := server.issues[number]
		if !found || server.issues[linked] == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		links := &issue.children
		if blockedBy {
			links = &issue.blockers
		}
		if slices.Contains(*links, linked) {
			http.Error(w, fmt.Sprintf("issue %d is already linked to %d", linked, number), http.StatusUnprocessableEntity)
			return
		}
		*links = append(*links, linked)
		w.WriteHeader(http.StatusCreated)
		writeFakeJSON(w, issueJSON(issue))
	})
}

// replayPublishBatchGitHub publishes validDecompositionPlan's two-child batch
// (the second child blocked by the first) under parent 419, which
// select-source has claimed, through the real stage entrypoint.
func replayPublishBatchGitHub(t *testing.T) replayFixture {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(replayPublishBatchParent, "An over-scoped issue", providers.LabelApproved, providers.LabelClaimed)
	server.mu.Lock()
	server.issues[replayPublishBatchParent].body = replayPublishBatchParentBody
	server.mu.Unlock()
	// select-source's claim, as a daemon run stores it.
	server.addComment(replayPublishBatchParent, fmt.Sprintf(
		"goobers-claim: run=%s\n\nClaimed by Goobers run `%s` for exactly-once processing.",
		replayPublishBatchRunID, replayPublishBatchRunID))
	// A human comment on the parent is not Goobers' and must be left alone.
	server.addCommentAs(replayPublishBatchParent, "maintainer", "Please split this into reviewable pieces.")

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	parent, err := server.newGitHubProvider("observer").GetWorkItem(context.Background(), repo, strconv.Itoa(replayPublishBatchParent))
	if err != nil {
		t.Fatalf("observe parent: %v", err)
	}
	plan := validDecompositionPlan(decomposition.Selection{
		Mode:        decomposition.SelectionModeEscalation,
		SourceRunID: "escalated-1",
		Parent: decomposition.ParentRef{
			Provider: string(providers.ProviderGitHub), Repository: "your-org/your-repo",
			ID: parent.ID, ObservedRevision: parent.Revision,
		},
	})
	digest, err := decomposition.PlanDigest(plan)
	if err != nil {
		t.Fatalf("digest plan: %v", err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", replayPublishBatchRunID)
	t.Setenv("GOOBERS_WORKFLOW", "decomposition")
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.InputEnvVar("planFile"), writeTopologyBInput(t, "plan.json", plan))
	t.Setenv(executor.InputEnvVar("validationFile"), writeTopologyBInput(t, "plan-validation.json",
		validatePlanResult{Valid: true, PlanDigest: digest}))

	counter := &providerWriteCounter{}
	graph := recordServerWrites(t, withGitHubIssueGraphWrites(server, server.server.Config.Handler), counter)
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = graph.URL })...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })
	t.Chdir(t.TempDir())

	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			code, stdout, stderr := runArgs(t, "publish-batch", root)
			if code == 0 && !strings.Contains(stdout, "published decomposition batch for parent 419 with 2 child(ren)") {
				return 1, stdout, "publish-batch did not publish the batch: " + stderr
			}
			return code, stdout, stderr
		},
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			return replayPublishBatchOwned(t, server, plan)
		},
	}
}

// replayPublishBatchOwned groups the batch's Goobers-owned artifacts by slot:
// each planned child, the marker comments on the parent and on each child,
// and the release of select-source's claim.
func replayPublishBatchOwned(t *testing.T, server *fakeGitHubServer, plan decomposition.Plan) map[string][]string {
	t.Helper()
	owned := map[string][]string{}
	childNumbers := map[string][]int{}
	server.mu.Lock()
	for _, number := range sortedIntKeys(server.issues) {
		body := server.issues[number].body
		parentID, _, key, marked, _ := decomposition.ChildBatchIdentity(body)
		if !marked || parentID != strconv.Itoa(replayPublishBatchParent) {
			continue
		}
		owned["child "+key] = append(owned["child "+key], body)
		childNumbers[key] = append(childNumbers[key], number)
	}
	server.mu.Unlock()

	parentSlots := map[string]func(string) bool{
		"prepared record": func(body string) bool { return strings.HasPrefix(body, decomposition.PreparedBatchMarkerPrefix) },
		"published record": func(body string) bool {
			return strings.HasPrefix(body, decomposition.PublishedBatchMarkerPrefix)
		},
		"parent note": func(body string) bool {
			return strings.HasPrefix(body, "Published this decomposition as a verified batch.")
		},
		"claim release": func(body string) bool {
			return strings.HasPrefix(body, "goobers-claim-release: run="+replayPublishBatchRunID+"\n")
		},
	}
	for slot, mine := range parentSlots {
		owned[slot] = ownedGitHubComments(t, server, replayPublishBatchParent, mine)
	}
	for _, child := range plan.Children {
		slot := "child " + child.Key + " note"
		owned[slot] = nil
		for _, number := range childNumbers[child.Key] {
			owned[slot] = append(owned[slot], ownedGitHubComments(t, server, number, func(body string) bool {
				return strings.HasPrefix(body, "This issue is child `"+child.Key+"`")
			})...)
		}
		if _, ok := owned["child "+child.Key]; !ok {
			owned["child "+child.Key] = nil
		}
	}
	return owned
}
