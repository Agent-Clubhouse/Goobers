package mergepolicy

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/providers"
)

// fakeRepoProvider implements providers.RepoProvider via an embedded nil
// interface (every method not overridden below panics if called, which is
// exactly what a misrouted Land dispatch should do in a test) — the same
// narrow-fake-over-full-mock philosophy internal/executor's PRPoller
// interface exists for, applied here since RepoProvider itself has no
// narrower carve-out for just the two methods Land calls.
type fakeRepoProvider struct {
	providers.RepoProvider
	mergeCalls    []providers.MergePullRequestRequest
	mergeResult   providers.MergePullRequestResult
	mergeErr      error
	enqueueCalls  []providers.EnqueuePullRequestRequest
	enqueueResult providers.EnqueuePullRequestResult
	enqueueErr    error
}

func (f *fakeRepoProvider) MergePullRequest(ctx context.Context, req providers.MergePullRequestRequest) (providers.MergePullRequestResult, error) {
	f.mergeCalls = append(f.mergeCalls, req)
	return f.mergeResult, f.mergeErr
}

func (f *fakeRepoProvider) EnqueuePullRequest(ctx context.Context, req providers.EnqueuePullRequestRequest) (providers.EnqueuePullRequestResult, error) {
	f.enqueueCalls = append(f.enqueueCalls, req)
	return f.enqueueResult, f.enqueueErr
}

func TestForPolicyDirectAndEmptyReturnDirectLander(t *testing.T) {
	for _, policy := range []providers.MergePolicy{providers.MergePolicyDirect, ""} {
		lander, err := ForPolicy(policy)
		if err != nil {
			t.Fatalf("ForPolicy(%q): unexpected error: %v", policy, err)
		}
		if _, ok := lander.(directLander); !ok {
			t.Fatalf("ForPolicy(%q) = %T, want directLander", policy, lander)
		}
	}
}

func TestForPolicyMergeQueueReturnsEnqueueLander(t *testing.T) {
	lander, err := ForPolicy(providers.MergePolicyMergeQueue)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := lander.(enqueueLander); !ok {
		t.Fatalf("ForPolicy(merge_queue) = %T, want enqueueLander", lander)
	}
}

func TestForPolicyUnknownErrors(t *testing.T) {
	if _, err := ForPolicy("carrier-pigeon"); err == nil {
		t.Fatal("expected an error for an unknown merge policy")
	}
}

func TestDirectLanderCallsMergePullRequest(t *testing.T) {
	fake := &fakeRepoProvider{mergeResult: providers.MergePullRequestResult{Merged: true, MergeSHA: "abc123"}}
	req := Request{
		Repository:      providers.RepositoryRef{Owner: "acme", Name: "widgets"},
		PullID:          "42",
		ExpectedHeadSHA: "deadbeef",
		CommitTitle:     "title",
		CommitMessage:   "message",
		MergeMethod:     providers.MergeMethodSquash,
	}
	result, err := directLander{}.Land(context.Background(), fake, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != OutcomeMerged || result.MergeSHA != "abc123" {
		t.Fatalf("Land result = %+v, want Outcome=merged MergeSHA=abc123", result)
	}
	if len(fake.mergeCalls) != 1 {
		t.Fatalf("MergePullRequest called %d times, want 1", len(fake.mergeCalls))
	}
	if len(fake.enqueueCalls) != 0 {
		t.Fatalf("EnqueuePullRequest called %d times, want 0 (direct policy must never enqueue)", len(fake.enqueueCalls))
	}
	got := fake.mergeCalls[0]
	if got.Repository != req.Repository || got.PullID != req.PullID || got.ExpectedHeadSHA != req.ExpectedHeadSHA ||
		got.CommitTitle != req.CommitTitle || got.CommitMessage != req.CommitMessage || got.MergeMethod != req.MergeMethod {
		t.Fatalf("MergePullRequest request = %+v, want fields threaded through from %+v", got, req)
	}
}

func TestDirectLanderPropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	fake := &fakeRepoProvider{mergeErr: wantErr}
	_, err := directLander{}.Land(context.Background(), fake, Request{PullID: "1"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Land error = %v, want %v", err, wantErr)
	}
}

func TestEnqueueLanderReportsEnqueuedWhenNotMerged(t *testing.T) {
	fake := &fakeRepoProvider{enqueueResult: providers.EnqueuePullRequestResult{Merged: false}}
	req := Request{Repository: providers.RepositoryRef{Owner: "acme", Name: "widgets"}, PullID: "7", ExpectedHeadSHA: "cafef00d", MergeMethod: providers.MergeMethodSquash}
	result, err := enqueueLander{}.Land(context.Background(), fake, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != OutcomeEnqueued || result.MergeSHA != "" {
		t.Fatalf("Land result = %+v, want Outcome=enqueued with no MergeSHA", result)
	}
	if len(fake.mergeCalls) != 0 {
		t.Fatalf("MergePullRequest called %d times, want 0 (enqueue policy must never direct-merge)", len(fake.mergeCalls))
	}
	if len(fake.enqueueCalls) != 1 {
		t.Fatalf("EnqueuePullRequest called %d times, want 1", len(fake.enqueueCalls))
	}
	got := fake.enqueueCalls[0]
	if got.Repository != req.Repository || got.PullID != req.PullID || got.ExpectedHeadSHA != req.ExpectedHeadSHA {
		t.Fatalf("EnqueuePullRequest request = %+v, want fields threaded through from %+v", got, req)
	}
	// #877: MergeMethod is threaded through here for the same reason
	// directLander threads it — the enqueue path is the merge endpoint, so
	// dropping it lets GitHub apply its own default and a restricted-method
	// ruleset rejects the landing outright.
	if got.MergeMethod != req.MergeMethod {
		t.Fatalf("EnqueuePullRequest MergeMethod = %q, want %q threaded through", got.MergeMethod, req.MergeMethod)
	}
}

// TestEnqueueLanderReportsMergedWhenQueueLandedImmediately pins the edge
// case documented on EnqueuePullRequestResult: an enqueue call that comes
// back merged=true (an empty queue landing the pull request immediately)
// must report Outcome=merged, not Outcome=enqueued — otherwise a caller
// would wrongly go on to watch a merge-queue entry that no longer exists.
func TestEnqueueLanderReportsMergedWhenQueueLandedImmediately(t *testing.T) {
	fake := &fakeRepoProvider{enqueueResult: providers.EnqueuePullRequestResult{Merged: true, MergeSHA: "f00dcafe"}}
	result, err := enqueueLander{}.Land(context.Background(), fake, Request{PullID: "7"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != OutcomeMerged || result.MergeSHA != "f00dcafe" {
		t.Fatalf("Land result = %+v, want Outcome=merged MergeSHA=f00dcafe", result)
	}
}

func TestEnqueueLanderPropagatesError(t *testing.T) {
	wantErr := errors.New("queue unavailable")
	fake := &fakeRepoProvider{enqueueErr: wantErr}
	_, err := enqueueLander{}.Land(context.Background(), fake, Request{PullID: "1"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Land error = %v, want %v", err, wantErr)
	}
}
