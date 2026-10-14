package prstatus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestVerdictStateBlocksEveryNonPass(t *testing.T) {
	tests := map[string]providers.CheckState{
		"pass":          providers.CheckStatePassing,
		"needs-changes": providers.CheckStateFailing,
		"fail":          providers.CheckStateFailing,
		"escalate":      providers.CheckStateFailing,
		"defer":         providers.CheckStatePending,
	}
	for decision, want := range tests {
		got, err := VerdictState(decision)
		if err != nil || got != want {
			t.Errorf("VerdictState(%q) = %q, %v; want %q", decision, got, err, want)
		}
	}
	if _, err := VerdictState("approve"); err == nil {
		t.Error("an unknown decision must not map to any state")
	}
}

func TestVerdictStatusRequestPinsHeadAndRecordsBase(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	req, err := VerdictStatusRequest(repo, "9", "needs-changes", "head-sha", "base-sha", "  Fix the\n loop bound. ", "https://verdict")
	if err != nil {
		t.Fatalf("VerdictStatusRequest: %v", err)
	}
	want := providers.PullRequestStatusRequest{
		Repository: repo, PullID: "9",
		Genre: providers.MergeReviewStatusGenre, Name: providers.MergeReviewStatusName,
		State:       providers.CheckStateFailing,
		Description: "needs-changes for base base-sha: Fix the loop bound.",
		TargetURL:   "https://verdict", HeadSHA: "head-sha",
	}
	if req != want {
		t.Fatalf("request = %+v, want %+v", req, want)
	}
	if _, err := VerdictStatusRequest(repo, "9", "pass", "", "base-sha", "", ""); err == nil {
		t.Fatal("a verdict status without the reviewed head must be refused, never posted on the live head")
	}
	if _, err := VerdictStatusRequest(repo, "9", "pass", "head-sha", "", "", ""); err == nil {
		t.Fatal("a verdict status without the reviewed base must be refused")
	}
}

func TestParseVerdictDescriptionRoundTrips(t *testing.T) {
	for _, summary := range []string{"", "ready"} {
		decision, base, ok := ParseVerdictDescription(VerdictDescription("pass", "base-sha", summary))
		if !ok || decision != "pass" || base != "base-sha" {
			t.Errorf("summary %q: parsed (%q, %q, %v), want (pass, base-sha, true)", summary, decision, base, ok)
		}
	}
	if _, _, ok := ParseVerdictDescription("goobers merge-review verdict: pass"); ok {
		t.Error("a description without a reviewed base must not parse")
	}
}

// TestVerdictStatusRefusalReproduces4188 is the merge-pr half of the #4188
// shape: ordinary CI is green, but the authoritative verdict status on the
// head says needs-changes, so merge-pr must refuse whatever its verdict input
// says. A later SHA-pinned pass for the pinned base clears it; a pass recorded
// for another base, or a pending stale status, does not authorize.
func TestVerdictStatusRefusalReproduces4188(t *testing.T) {
	status := func(state providers.CheckState, description string) *providers.CheckDetail {
		return &providers.CheckDetail{Name: providers.MergeReviewStatusContext, State: state, Summary: description}
	}
	tests := []struct {
		name       string
		status     *providers.CheckDetail
		wantRefuse string
	}{
		{"needs-changes blocks", status(providers.CheckStateFailing, VerdictDescription("needs-changes", "base", "fix it")), "is failure"},
		{"defer blocks", status(providers.CheckStatePending, VerdictDescription("defer", "base", "after #1")), "is pending"},
		{"stale blocks", status(providers.CheckStatePending, VerdictDescription("stale", "base", "re-review pending")), "is pending"},
		{"pass for an older base does not authorize", status(providers.CheckStatePassing, VerdictDescription("pass", "old-base", "")), "is a pass for base old-base"},
		{"pass without a base does not authorize", status(providers.CheckStatePassing, "goobers merge-review verdict: pass"), "names no reviewed base"},
		{"pinned pass clears", status(providers.CheckStatePassing, VerdictDescription("pass", "base", "ready")), ""},
		{"no status is left to the provider's required check", nil, ""},
	}
	for _, tt := range tests {
		got := VerdictStatusRefusal(tt.status, "head", "base")
		if tt.wantRefuse == "" {
			if got != "" {
				t.Errorf("%s: refusal = %q, want none", tt.name, got)
			}
			continue
		}
		if !strings.Contains(got, tt.wantRefuse) || !strings.Contains(got, "head head") {
			t.Errorf("%s: refusal = %q, want it to name the head and contain %q", tt.name, got, tt.wantRefuse)
		}
	}
}

func TestStaleVerdictStatusRequestIsPendingOnTheSameHead(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "acme", Name: "app"}
	req := StaleVerdictStatusRequest(repo, "9", "head", "new-base", "PR base moved", "https://pr")
	if req.State != providers.CheckStatePending || req.HeadSHA != "head" || req.Name != providers.MergeReviewStatusName {
		t.Fatalf("request = %+v, want a pending merge-review status on the reviewed head", req)
	}
	decision, base, ok := ParseVerdictDescription(req.Description)
	if !ok || decision != "stale" || base != "new-base" {
		t.Fatalf("description %q parsed (%q, %q, %v)", req.Description, decision, base, ok)
	}
}

// readingPublisher is a status publisher that can also read the verdict
// status standing on the head, as GitHub and Gitea can.
type readingPublisher struct {
	standing *providers.CheckDetail
	posted   []providers.PullRequestStatusRequest
}

func (p *readingPublisher) PublishPullRequestStatus(_ context.Context, req providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error) {
	p.posted = append(p.posted, req)
	return providers.PullRequestStatusResult{ID: len(p.posted)}, nil
}

func (p *readingPublisher) MergeReviewStatus(context.Context, providers.RepositoryRef, string) (*providers.CheckDetail, error) {
	return p.standing, nil
}

// TestPublishVerdictDegradesOnlyAMissingGrant: a provider without the status
// surface warns; any failure other than a refused grant or the status cap is
// returned, so the verdict is never reported published without its status.
func TestPublishVerdictDegradesOnlyAMissingGrant(t *testing.T) {
	req := providers.PullRequestStatusRequest{Genre: "goobers", Name: "merge-review", HeadSHA: "head"}
	if result, err := PublishVerdict(context.Background(), struct{}{}, req); err != nil || result.Warning == "" {
		t.Fatalf("non-publisher: %+v, err %v; want a warning", result, err)
	}
	ok := testPublisher{publish: func(context.Context, providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error) {
		return providers.PullRequestStatusResult{ID: 1}, nil
	}}
	if result, err := PublishVerdict(context.Background(), ok, req); err != nil || result != (VerdictPublication{}) {
		t.Fatalf("published: %+v, err %v; want a plain publication", result, err)
	}
	transient := testPublisher{publish: func(context.Context, providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error) {
		return providers.PullRequestStatusResult{}, errors.New("connection reset")
	}}
	if _, err := PublishVerdict(context.Background(), transient, req); err == nil || !strings.Contains(err.Error(), "goobers/merge-review") {
		t.Fatalf("transient failure: err %v, want it returned naming the context", err)
	}
}

// TestPublishVerdictSkipsAnUnchangedVerdict: a head re-reviewed every tick
// must not accumulate a status per run toward GitHub's per-context cap.
func TestPublishVerdictSkipsAnUnchangedVerdict(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	req, err := VerdictStatusRequest(repo, "9", "pass", "head", "base", "ready", "")
	if err != nil {
		t.Fatal(err)
	}
	same := &readingPublisher{standing: &providers.CheckDetail{
		Name: providers.MergeReviewStatusContext, State: req.State, Summary: req.Description,
	}}
	if result, err := PublishVerdict(context.Background(), same, req); err != nil || !result.Unchanged || len(same.posted) != 0 {
		t.Fatalf("unchanged verdict: %+v, err %v, posted %d; want nothing posted", result, err, len(same.posted))
	}
	changed := &readingPublisher{standing: &providers.CheckDetail{
		Name: providers.MergeReviewStatusContext, State: providers.CheckStateFailing,
		Summary: VerdictDescription("needs-changes", "base", "fix"),
	}}
	if result, err := PublishVerdict(context.Background(), changed, req); err != nil || result.Unchanged || len(changed.posted) != 1 {
		t.Fatalf("changed verdict: %+v, err %v, posted %d; want one post", result, err, len(changed.posted))
	}
}

// TestWithdrawVerdictOnlyWithdrawsAStandingVerdict: a head with no verdict
// status gets no pending stale status.
func TestWithdrawVerdictOnlyWithdrawsAStandingVerdict(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	req := StaleVerdictStatusRequest(repo, "9", "head", "new-base", "PR base moved", "")
	none := &readingPublisher{}
	if result, err := WithdrawVerdict(context.Background(), none, req); err != nil || !result.Unchanged || len(none.posted) != 0 {
		t.Fatalf("no standing verdict: %+v, err %v, posted %d; want nothing posted", result, err, len(none.posted))
	}
	standing := &readingPublisher{standing: &providers.CheckDetail{
		Name: providers.MergeReviewStatusContext, State: providers.CheckStatePassing,
		Summary: VerdictDescription("pass", "old-base", "ready"),
	}}
	if result, err := WithdrawVerdict(context.Background(), standing, req); err != nil || result.Unchanged || len(standing.posted) != 1 {
		t.Fatalf("standing pass: %+v, err %v, posted %d; want it withdrawn", result, err, len(standing.posted))
	}
}

// TestPublishVerdictDegradesAtTheStatusCap: GitHub's per-context status cap is
// a warning, not a stage failure that recurs on every later review.
func TestPublishVerdictDegradesAtTheStatusCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"success","statuses":[]}`))
			return
		}
		http.Error(w, `{"message":"Validation Failed","errors":[{"resource":"Status","code":"custom","message":"This SHA and context has reached the maximum number of statuses."}]}`, http.StatusUnprocessableEntity)
	}))
	t.Cleanup(server.Close)
	github := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) { p.BaseURL = server.URL })
	req, err := VerdictStatusRequest(providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}, "9", "pass", "head", "base", "ready", "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := PublishVerdict(context.Background(), github, req)
	if err != nil || !strings.Contains(result.Warning, "cap") {
		t.Fatalf("status cap: %+v, err %v; want a warning", result, err)
	}
}

func TestVerdictDescriptionIsBounded(t *testing.T) {
	description := VerdictDescription("needs-changes", strings.Repeat("a", 40), strings.Repeat("é", 300))
	if n := len([]rune(description)); n != DescriptionLimit {
		t.Fatalf("description is %d runes, want %d", n, DescriptionLimit)
	}
	if _, base, ok := ParseVerdictDescription(description); !ok || base != strings.Repeat("a", 40) {
		t.Fatalf("a bounded description must keep its base: %q", description)
	}
}

func TestVerdictSummaryFallsBackToRationale(t *testing.T) {
	if got := VerdictSummary(" ready ", ""); got != "ready" {
		t.Errorf("summary = %q", got)
	}
	if got := VerdictSummary("", "\nFirst line.\nSecond."); got != "First line." {
		t.Errorf("rationale fallback = %q", got)
	}
}
