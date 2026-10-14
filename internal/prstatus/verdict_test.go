package prstatus

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestVerdictStateBlocksEveryNonPass(t *testing.T) {
	tests := map[apiv1.VerdictDecision]providers.CheckState{
		apiv1.VerdictPass:         providers.CheckStatePassing,
		apiv1.VerdictNeedsChanges: providers.CheckStateFailing,
		apiv1.VerdictFail:         providers.CheckStateFailing,
		apiv1.VerdictEscalate:     providers.CheckStateFailing,
		apiv1.VerdictDefer:        providers.CheckStatePending,
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
	req, err := VerdictStatusRequest(repo, "9", apiv1.VerdictNeedsChanges, "head-sha", "base-sha", "  Fix the\n loop bound. ", "https://verdict")
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
	if _, err := VerdictStatusRequest(repo, "9", apiv1.VerdictPass, "", "base-sha", "", ""); err == nil {
		t.Fatal("a verdict status without the reviewed head must be refused, never posted on the live head")
	}
	if _, err := VerdictStatusRequest(repo, "9", apiv1.VerdictPass, "head-sha", "", "", ""); err == nil {
		t.Fatal("a verdict status without the reviewed base must be refused")
	}
}

func TestParseVerdictDescriptionRoundTrips(t *testing.T) {
	for _, summary := range []string{"", "ready"} {
		decision, base, ok := ParseVerdictDescription(VerdictDescription(apiv1.VerdictPass, "base-sha", summary))
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
		{"needs-changes blocks", status(providers.CheckStateFailing, VerdictDescription(apiv1.VerdictNeedsChanges, "base", "fix it")), "is failure"},
		{"defer blocks", status(providers.CheckStatePending, VerdictDescription(apiv1.VerdictDefer, "base", "after #1")), "is pending"},
		{"stale blocks", status(providers.CheckStatePending, VerdictDescription("stale", "base", "re-review pending")), "is pending"},
		{"pass for an older base does not authorize", status(providers.CheckStatePassing, VerdictDescription(apiv1.VerdictPass, "old-base", "")), "is a pass for base old-base"},
		{"pass without a base does not authorize", status(providers.CheckStatePassing, "goobers merge-review verdict: pass"), "names no reviewed base"},
		{"pinned pass clears", status(providers.CheckStatePassing, VerdictDescription(apiv1.VerdictPass, "base", "ready")), ""},
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

// TestPublishVerdictDegradesOnlyAMissingGrant: a provider without the status
// surface warns; any failure other than a refused grant is returned, so the
// verdict is never reported published without its status.
func TestPublishVerdictDegradesOnlyAMissingGrant(t *testing.T) {
	req := providers.PullRequestStatusRequest{Genre: "goobers", Name: "merge-review", HeadSHA: "head"}
	if warning, err := PublishVerdict(context.Background(), struct{}{}, req); err != nil || warning == "" {
		t.Fatalf("non-publisher: warning %q, err %v; want a warning", warning, err)
	}
	ok := testPublisher{publish: func(context.Context, providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error) {
		return providers.PullRequestStatusResult{ID: 1}, nil
	}}
	if warning, err := PublishVerdict(context.Background(), ok, req); err != nil || warning != "" {
		t.Fatalf("published: warning %q, err %v; want neither", warning, err)
	}
	transient := testPublisher{publish: func(context.Context, providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error) {
		return providers.PullRequestStatusResult{}, errors.New("connection reset")
	}}
	if _, err := PublishVerdict(context.Background(), transient, req); err == nil || !strings.Contains(err.Error(), "goobers/merge-review") {
		t.Fatalf("transient failure: err %v, want it returned naming the context", err)
	}
}

func TestVerdictSummaryFallsBackToRationale(t *testing.T) {
	if got := VerdictSummary(apiv1.Verdict{Summary: " ready "}); got != "ready" {
		t.Errorf("summary = %q", got)
	}
	if got := VerdictSummary(apiv1.Verdict{Rationale: "\nFirst line.\nSecond."}); got != "First line." {
		t.Errorf("rationale fallback = %q", got)
	}
}
