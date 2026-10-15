package providers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/claimrelease"
)

// legacyClaimReleaseBreadcrumb is the release comment every provider posted
// before #4421. Historical comments in this shape must keep closing their
// claim epoch.
func legacyClaimReleaseBreadcrumb(runID string) string {
	return fmt.Sprintf("goobers-claim-release: run=%s\n\nReleased by Goobers run `%s`; a later run may claim this item.", runID, runID)
}

func commentTexts(comments []map[string]interface{}, field string) []string {
	var texts []string
	for _, comment := range comments {
		if text, ok := comment[field].(string); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

func releaseComments(texts []string) []string {
	var released []string
	for _, text := range texts {
		if claimReleaseRunID(text) != "" {
			released = append(released, text)
		}
	}
	return released
}

// assertReleaseComment pins the provider-visible contract: the historical
// boundary still parses to the released run, and the comment states the
// bounded human reason beside the versioned machine marker.
func assertReleaseComment(t *testing.T, body, runID string, reason claimrelease.Reason) {
	t.Helper()
	if got := claimReleaseRunID(body); got != runID {
		t.Fatalf("release boundary run = %q, want %q in\n%s", got, runID, body)
	}
	if !strings.Contains(body, reason.Sentence()) || !strings.Contains(body, reason.Eligibility()) {
		t.Fatalf("release comment lacks the human reason %q in\n%s", reason.Sentence(), body)
	}
	if !strings.Contains(body, "\n"+reason.MarkerLine()+"\n") {
		t.Fatalf("release comment lacks its own marker line %q in\n%s", reason.MarkerLine(), body)
	}
}

func TestReleaseCommentsCarryTheReasonOnEveryProvider(t *testing.T) {
	ctx := context.Background()
	reason := claimrelease.Escalated.Because("DEPENDENCY_NOT_MET", "registry access is unavailable").At("implement")
	retry := claimrelease.Completed.Because("contradiction", "")

	t.Run("github", func(t *testing.T) {
		m := newIssueMock()
		p, repo := newIssueProvider(t, m)
		if res, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"}); err != nil || !res.Claimed {
			t.Fatalf("claim = %+v, %v", res, err)
		}
		for _, r := range []claimrelease.Reason{reason, retry} {
			if _, err := p.ReleaseWorkItemClaim(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A", Reason: r}); err != nil {
				t.Fatal(err)
			}
		}
		released := releaseComments(commentTexts(m.comments, "body"))
		if len(released) != 1 {
			t.Fatalf("release comments = %d, want one boundary despite the retry", len(released))
		}
		assertReleaseComment(t, released[0], "run-A", reason)
	})
	t.Run("gitea", func(t *testing.T) {
		m := newGiteaIssueMock()
		p, repo := newGiteaIssueProvider(t, m)
		if res, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"}); err != nil || !res.Claimed {
			t.Fatalf("claim = %+v, %v", res, err)
		}
		for _, r := range []claimrelease.Reason{reason, retry} {
			if _, err := p.ReleaseWorkItemClaim(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A", Reason: r}); err != nil {
				t.Fatal(err)
			}
		}
		released := releaseComments(commentTexts(m.comments, "body"))
		if len(released) != 1 {
			t.Fatalf("release comments = %d, want one boundary despite the retry", len(released))
		}
		assertReleaseComment(t, released[0], "run-A", reason)
	})
	t.Run("ado", func(t *testing.T) {
		fake := &adoClaimFake{tags: "goobers:approved; " + LabelClaimed}
		fake.seed(adoTestSelfID, claimBreadcrumb("run-42"))
		server := fake.server(t, true)
		provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
		repo := RepositoryRef{Provider: ProviderADO, Name: "repo", Project: "project"}
		for _, r := range []claimrelease.Reason{reason, retry} {
			if _, err := provider.ReleaseWorkItemClaim(ctx, ClaimWorkItemRequest{Repository: repo, ID: "42", RunID: "run-42", Reason: r}); err != nil {
				t.Fatal(err)
			}
		}
		fake.mu.Lock()
		released := releaseComments(commentTexts(fake.comments, "text"))
		fake.mu.Unlock()
		if len(released) != 1 {
			t.Fatalf("release comments = %d, want one boundary despite the retry", len(released))
		}
		assertReleaseComment(t, released[0], "run-42", reason)
	})
}

// TestClaimReleaseClaimSequenceExplainsEachTransition is the flapping case the
// issue was filed about: the same item claimed and released repeatedly must
// read, from the issue timeline alone, as distinct, explained transitions.
func TestClaimReleaseClaimSequenceExplainsEachTransition(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	ctx := context.Background()
	steps := []struct {
		run    string
		reason claimrelease.Reason
	}{
		{"run-A", claimrelease.Failed.Because("agent_exit", "agent exited 2").At("implement")},
		{"run-B", claimrelease.StallSwept.Because("lease_expired", "")},
		{"run-C", claimrelease.Completed.Because("done", "")},
	}
	for _, step := range steps {
		if res, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: step.run}); err != nil || !res.Claimed {
			t.Fatalf("claim by %s = %+v, %v", step.run, res, err)
		}
		if _, err := p.ReleaseWorkItemClaim(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: step.run, Reason: step.reason}); err != nil {
			t.Fatal(err)
		}
	}
	released := releaseComments(commentTexts(m.comments, "body"))
	if len(released) != len(steps) {
		t.Fatalf("release comments = %d, want %d", len(released), len(steps))
	}
	for i, step := range steps {
		assertReleaseComment(t, released[i], step.run, step.reason)
	}
}

func TestReleaseCommentRedactsAndBoundsTheSummary(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	ctx := context.Background()
	if res, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"}); err != nil || !res.Claimed {
		t.Fatalf("claim = %+v, %v", res, err)
	}
	secret := "ghp_" + strings.Repeat("Z", 36)
	summary := "push rejected with " + secret + " from /Users/operator/src/repo/.git " + strings.Repeat("noise ", 200)
	reason := claimrelease.Failed.Because("push_rejected", summary).At("publish")
	if _, err := p.ReleaseWorkItemClaim(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A", Reason: reason}); err != nil {
		t.Fatal(err)
	}
	body := releaseComments(commentTexts(m.comments, "body"))[0]
	for _, leaked := range []string{secret, "/Users/operator"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("release comment leaked %q:\n%s", leaked, body)
		}
	}
	if !strings.Contains(body, "push_rejected") || strings.Count(body, "noise") > claimrelease.SummaryLimit/len("noise ") {
		t.Fatalf("release comment lost the code or exceeded the summary bound:\n%s", body)
	}
}

// A summary is provider- and agent-influenced text. Whatever it contains, the
// release comment must still be attributable and postable: a refused comment
// leaves the claim epoch open and the item stuck as claimed.
func TestReleaseCommentSurvivesHostileSummaries(t *testing.T) {
	attribution := Attribution{
		InstanceID: "e62c1c105fdc4273a72d199394b41cb0", Gaggle: "example", Workflow: "implementation",
		Task: "close-out", Goober: "implementer", Run: "224712dcde5c4deda9717a03a8c26770",
	}
	fullMarker, err := withAttribution("x", attribution, "verdict")
	if err != nil {
		t.Fatal(err)
	}
	for name, summary := range map[string]string{
		"unclosed marker":    "agent said " + AttributionMarkerPrefix + "AAAA",
		"truncated marker":   strings.Repeat("y", 200) + " " + fullMarker,
		"html and a mention": "ping @octocat and <img src=x>",
	} {
		t.Run(name, func(t *testing.T) {
			body, err := withAttribution(claimReleaseBreadcrumb("run-A", claimrelease.Failed.Because("agent_exit", summary)), attribution, "claim-release")
			if err != nil {
				t.Fatalf("release comment refused: %v", err)
			}
			human := body[:strings.Index(body, AttributionMarkerPrefix)]
			if strings.ContainsAny(human, "<>@") {
				t.Fatalf("release comment carries raw HTML or a mention:\n%s", human)
			}
			if claimReleaseRunID(body) != "run-A" {
				t.Fatalf("release boundary lost:\n%s", body)
			}
		})
	}
}

func TestLegacyReleaseCommentStillClosesTheEpoch(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	ctx := context.Background()
	if res, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-old"}); err != nil || !res.Claimed {
		t.Fatalf("claim = %+v, %v", res, err)
	}
	if err := p.postComment(ctx, repo, "7", legacyClaimReleaseBreadcrumb("run-old")); err != nil {
		t.Fatal(err)
	}
	next, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-new"})
	if err != nil || !next.Claimed || next.ClaimedBy != "run-new" {
		t.Fatalf("claim after a historical release = %+v, %v", next, err)
	}
}
