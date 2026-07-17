// Package mergepolicy dispatches a repo's approved-merge landing operation
// (issue #758) to the correct provider action for that repo's detected
// merge policy — direct-merge or merge-queue-enqueue — so merge-review/
// pr-remediation/auto-merge code never branches on which one is in effect.
// This is the same interface-dispatch shape providers.Provider already uses
// to select a concrete backend (GitHub vs. ADO) by ProviderKind: one seam
// (Land), concrete implementations (directLander/enqueueLander) selected by
// a runtime, per-repo fact (MergePolicy) rather than scattered branching at
// every call site. Extending to a third policy is a new Lander
// implementation registered in ForPolicy, not a rewrite of any existing one.
package mergepolicy

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/providers"
)

// Outcome normalizes what Land actually did to the pull request.
type Outcome string

// Outcomes a Lander can report. Both are successful landings — the
// difference is whether the pull request is already merged or only queued
// to be — never a failure; a genuine error is returned as the error instead.
const (
	OutcomeMerged   Outcome = "merged"
	OutcomeEnqueued Outcome = "enqueued"
)

// Request is the provider-neutral landing request every Lander
// implementation receives — the fields MergePullRequestRequest and
// EnqueuePullRequestRequest both need, so mergepr.go's poll->decide->merge
// closure builds this once regardless of which policy is active.
type Request struct {
	Repository      providers.RepositoryRef
	PullID          string
	ExpectedHeadSHA string
	CommitTitle     string
	CommitMessage   string
	MergeMethod     providers.MergeMethod
}

// Result reports what Land did.
type Result struct {
	Outcome  Outcome
	MergeSHA string
}

// Lander lands an already-conjunct-verified pull request. It performs no
// policy check of its own — matching MergePullRequestRequest/
// EnqueuePullRequestRequest's own doc convention — the caller (mergepr.go's
// poll->decide->merge closure) is responsible for verifying every merge
// conjunct (verdict=pass, CI green, not draft, SHA-pin) before calling Land.
type Lander interface {
	Land(ctx context.Context, provider providers.RepoProvider, req Request) (Result, error)
}

// directLander lands by calling the provider's direct merge API — today's
// only behavior, unchanged for a repo whose detected policy is direct.
type directLander struct{}

func (directLander) Land(ctx context.Context, provider providers.RepoProvider, req Request) (Result, error) {
	res, err := provider.MergePullRequest(ctx, providers.MergePullRequestRequest{
		Repository:      req.Repository,
		PullID:          req.PullID,
		ExpectedHeadSHA: req.ExpectedHeadSHA,
		CommitTitle:     req.CommitTitle,
		CommitMessage:   req.CommitMessage,
		MergeMethod:     req.MergeMethod,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Outcome: OutcomeMerged, MergeSHA: res.MergeSHA}, nil
}

// enqueueLander lands by adding the pull request to its repo's merge queue.
type enqueueLander struct{}

func (enqueueLander) Land(ctx context.Context, provider providers.RepoProvider, req Request) (Result, error) {
	res, err := provider.EnqueuePullRequest(ctx, providers.EnqueuePullRequestRequest{
		Repository:      req.Repository,
		PullID:          req.PullID,
		ExpectedHeadSHA: req.ExpectedHeadSHA,
	})
	if err != nil {
		return Result{}, err
	}
	if res.Merged {
		// The queue's own enqueue endpoint completed the merge immediately
		// (e.g. nothing else ahead of this pull request) — a genuine
		// "merged" outcome, not "enqueued"; see EnqueuePullRequestResult's
		// doc.
		return Result{Outcome: OutcomeMerged, MergeSHA: res.MergeSHA}, nil
	}
	return Result{Outcome: OutcomeEnqueued}, nil
}

// ForPolicy returns the Lander for policy. An empty policy is treated as
// direct — the pre-#758 behavior every repo effectively had before merge
// policy detection existed at all, so a caller that (for whatever reason)
// could not resolve a policy fails toward today's behavior rather than
// toward an error. A third policy is a new Lander implementation plus one
// case here, never a change to either existing one or to any call site.
func ForPolicy(policy providers.MergePolicy) (Lander, error) {
	switch policy {
	case providers.MergePolicyMergeQueue:
		return enqueueLander{}, nil
	case providers.MergePolicyDirect, "":
		return directLander{}, nil
	default:
		return nil, fmt.Errorf("mergepolicy: unknown merge policy %q", policy)
	}
}
