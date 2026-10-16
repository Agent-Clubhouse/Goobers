package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/providers"
)

type prRetryFake struct {
	remediationProvider
	pr         providers.PullRequestSummary
	comments   []providers.Comment
	updates    []providers.UpdateWorkItemRequest
	edits      map[string]string
	dropLabels bool
}

func (f *prRetryFake) GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error) {
	return f.pr, nil
}

func (f *prRetryFake) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return f.comments, nil
}

func (f *prRetryFake) UpdateComment(_ context.Context, _ providers.RepositoryRef, id, body string) error {
	if f.edits == nil {
		f.edits = map[string]string{}
	}
	f.edits[id] = body
	return nil
}

func (f *prRetryFake) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.updates = append(f.updates, req)
	if !f.dropLabels {
		f.pr.Labels = prRetryResultLabels(f.pr.Labels, req.AddLabels, req.RemoveLabels)
	}
	return providers.WorkItem{}, nil
}

func escalatedFake(t *testing.T, labels ...string) *prRetryFake {
	t.Helper()
	body, err := remediationStateComment(remediationState{
		Cycles: 3, Escalated: true, EscalatedHeadSHA: "h1", EscalatedBaseSHA: "b1", EscalationGeneration: 2,
		LastDiffDigest: "d", AttemptsByCause: remediationAttempts{Conflict: 2}, RemediationAttempted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &prRetryFake{
		pr:       providers.PullRequestSummary{Number: 533, State: "open", HeadSHA: "h1", BaseSHA: "b1", Labels: labels},
		comments: []providers.Comment{{ID: "c1", Body: body}},
	}
}

func retryParams(route string) prRetryParams {
	return prRetryParams{Number: 533, Route: route, Reason: "contract fixed", Actor: "op"}
}

func noClaim() (claimsclient.Entry, bool, error) { return claimsclient.Entry{}, false, nil }

func TestPRRetryRemediationAppliesAndResetsState(t *testing.T) {
	f := escalatedFake(t, providers.LabelMergeEscalated, "type:x")
	report, err := executePRRetry(context.Background(), f, providers.RepositoryRef{}, noClaim, retryParams(prRetryRouteRemediation), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Applied || report.Eligibility != "eligible for pr-remediation" {
		t.Fatalf("report = %+v", report)
	}
	if !sameLabelSet(f.pr.Labels, []string{needsRemediationLabel, "type:x"}) {
		t.Fatalf("labels = %v", f.pr.Labels)
	}
	state, ok := parseRemediationStateComment(f.edits["c1"])
	if !ok || state.Escalated || state.AttemptsByCause != (remediationAttempts{}) || state.LastDiffDigest != "" || state.EscalatedHeadSHA != "" {
		t.Fatalf("state not reset: %+v", state)
	}
	if state.EscalationGeneration != 2 || state.OperatorRetry == nil || state.OperatorRetry.Actor != "op" || state.OperatorRetry.Reason != "contract fixed" {
		t.Fatalf("audit/generation lost: %+v", state)
	}
	last := f.updates[len(f.updates)-1]
	if !strings.Contains(last.Comment, "pr-retry-audit") || !strings.Contains(last.Comment, "contract fixed") {
		t.Fatalf("audit comment = %q", last.Comment)
	}
}

func TestPRRetryIsIdempotent(t *testing.T) {
	f := escalatedFake(t, providers.LabelMergeEscalated)
	if _, err := executePRRetry(context.Background(), f, providers.RepositoryRef{}, noClaim, retryParams(prRetryRouteMergeReview), time.Now); err != nil {
		t.Fatal(err)
	}
	f.comments = []providers.Comment{{ID: "c1", Body: f.edits["c1"]}}
	report, err := executePRRetry(context.Background(), f, providers.RepositoryRef{}, noClaim, retryParams(prRetryRouteMergeReview), time.Now)
	if err == nil {
		t.Fatalf("second run on a healthy PR should refuse, got %+v", report)
	}
	f.pr.Labels = []string{needsRemediationLabel}
	if _, err := executePRRetry(context.Background(), f, providers.RepositoryRef{}, noClaim, retryParams(prRetryRouteRemediation), time.Now); err != nil {
		t.Fatal(err)
	}
	report, err = executePRRetry(context.Background(), f, providers.RepositoryRef{}, noClaim, retryParams(prRetryRouteRemediation), time.Now)
	if err != nil || !report.NoOp || report.Applied {
		t.Fatalf("report = %+v err = %v", report, err)
	}
	before := len(f.updates)
	_, _ = executePRRetry(context.Background(), f, providers.RepositoryRef{}, noClaim, retryParams(prRetryRouteRemediation), time.Now)
	if len(f.updates) != before {
		t.Fatal("no-op retry mutated the provider")
	}
}

func TestPRRetryDryRunDoesNotMutate(t *testing.T) {
	f := escalatedFake(t, providers.LabelMergeEscalated)
	p := retryParams(prRetryRouteMergeReview)
	p.DryRun = true
	report, err := executePRRetry(context.Background(), f, providers.RepositoryRef{}, noClaim, p, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 0 || len(f.edits) != 0 || report.Applied {
		t.Fatalf("dry run mutated: %+v %+v", f.updates, f.edits)
	}
	if !report.Checkpoint.Found || report.Checkpoint.EscalationGeneration != 2 || report.HeadSHA != "h1" || !report.ResetState {
		t.Fatalf("report = %+v", report)
	}
	var out strings.Builder
	renderPRRetryReport(&out, report)
	for _, want := range []string{"dry run", "generation=2", "head=h1", "eligible for merge-review"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestPRRetryRefusals(t *testing.T) {
	live := func() (claimsclient.Entry, bool, error) {
		return claimsclient.Entry{RunID: "r1", ExpiresAt: time.Now().Add(time.Hour)}, true, nil
	}
	cases := []struct {
		name   string
		mutate func(*prRetryFake)
		claim  func() (claimsclient.Entry, bool, error)
		route  string
	}{
		{"live claim", nil, live, prRetryRouteRemediation},
		{"merged", func(f *prRetryFake) { f.pr.Merged = true }, noClaim, prRetryRouteHold},
		{"closed", func(f *prRetryFake) { f.pr.State = "closed" }, noClaim, prRetryRouteRemediation},
		{"healthy", func(f *prRetryFake) { f.pr.Labels = nil }, noClaim, prRetryRouteRemediation},
		{"failing ci to merge-review", func(f *prRetryFake) { f.pr.CheckState = providers.CheckStateFailing }, noClaim, prRetryRouteMergeReview},
		{"ambiguous", func(f *prRetryFake) {
			f.pr.Labels = []string{needsRemediationLabel, blockedOnSiblingLabel}
		}, noClaim, prRetryRouteMergeReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := escalatedFake(t, providers.LabelMergeEscalated)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			_, err := executePRRetry(context.Background(), f, providers.RepositoryRef{}, tc.claim, retryParams(tc.route), time.Now)
			var refusal prRetryRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want refusal", err)
			}
			if len(f.updates) != 0 || len(f.edits) != 0 {
				t.Fatal("refused retry mutated the provider")
			}
		})
	}
}

func TestPRRetryExpiredClaimDoesNotBlock(t *testing.T) {
	f := escalatedFake(t, providers.LabelMergeEscalated)
	expired := func() (claimsclient.Entry, bool, error) {
		return claimsclient.Entry{RunID: "r1", ExpiresAt: time.Now().Add(-time.Hour)}, true, nil
	}
	if _, err := executePRRetry(context.Background(), f, providers.RepositoryRef{}, expired, retryParams(prRetryRouteRemediation), time.Now); err != nil {
		t.Fatal(err)
	}
}

func TestPRRetryHoldRoute(t *testing.T) {
	f := escalatedFake(t, needsRemediationLabel)
	report, err := executePRRetry(context.Background(), f, providers.RepositoryRef{}, noClaim, retryParams(prRetryRouteHold), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if !sameLabelSet(f.pr.Labels, []string{providers.LabelMergeEscalated, providers.LabelNeedsHuman}) || !strings.HasPrefix(report.Eligibility, "held") {
		t.Fatalf("labels = %v report = %+v", f.pr.Labels, report)
	}
}

func TestPRRetryReadAfterWriteMismatchFails(t *testing.T) {
	f := escalatedFake(t, providers.LabelMergeEscalated)
	f.dropLabels = true
	_, err := executePRRetry(context.Background(), f, providers.RepositoryRef{}, noClaim, retryParams(prRetryRouteRemediation), time.Now)
	if err == nil || !strings.Contains(err.Error(), "read-after-write") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunPRRetryUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--route", "remediation", "--reason", "x"},
		{"--pr", "1", "--route", "bogus", "--reason", "x"},
		{"--pr", "1", "--route", "hold"},
	} {
		var out, errb strings.Builder
		if code := runPRRetry(args, &out, &errb); code != 2 {
			t.Fatalf("args %v: code = %d, stderr = %s", args, code, errb.String())
		}
	}
}
