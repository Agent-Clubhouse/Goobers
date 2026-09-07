package executor

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// lifecyclePoller returns a fixed poll result and counts calls, so a test can
// prove ci-poll stopped after ONE provider call rather than sleeping and
// retrying to its overall timeout.
type lifecyclePoller struct {
	result providers.PullRequestPollResult
	calls  int
}

func (p *lifecyclePoller) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	p.calls++
	return p.result, nil
}

// TestCIPollStopsWhenThePullRequestWasClosedWithoutMerging is #2786's
// regression pin.
//
// ci-poll only ever asked about check state, and an abandoned pull request's
// policy evaluations can stay queued indefinitely — so the stage stayed
// healthy, emitted heartbeats and slept until its overall timeout while
// holding its workspace, claim and a runner. Live on ADO, run
// d2bde587afd546016d7cec69d37d3d68 polled PR 2331139 for over 21 minutes after
// it was abandoned by hand and stopped only when the run was cancelled.
func TestCIPollStopsWhenThePullRequestWasClosedWithoutMerging(t *testing.T) {
	poller := &lifecyclePoller{result: providers.PullRequestPollResult{
		State:      "closed",
		Merged:     false,
		CheckState: providers.CheckStatePending,
	}}
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = func(context.Context, time.Duration) error {
		t.Fatal("ci-poll slept on a closed pull request instead of stopping")
		return nil
	}

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "2331139"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if poller.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", poller.calls)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "pull_request_closed" {
		t.Fatalf("error = %+v, want code pull_request_closed", result.Error)
	}
	// Non-retryable on purpose: nothing about this run can reopen the pull
	// request, so a retry only spends another runner reaching the same answer.
	if result.Error.Retryable {
		t.Fatal("error is retryable; retrying cannot reopen a closed pull request")
	}
	// Not ciStatus=failing on purpose: that branch repasses to implementation,
	// which would open a replacement for the pull request an operator just
	// chose to abandon.
	if got := result.Outputs[OutputCIStatus]; got != CIStatusClosed {
		t.Fatalf("outputs[%s] = %v, want %q — the failing branch would repass to implementation "+
			"and open a replacement PR", OutputCIStatus, got, CIStatusClosed)
	}
	if got := result.Outputs[OutputPRNumber]; got != "2331139" {
		t.Fatalf("outputs[%s] = %v, want the polled PR", OutputPRNumber, got)
	}
}

// TestCIPollStopsWhenThePullRequestWasMerged pins the other lifecycle exit the
// issue asks to define explicitly. The checks ci-poll was waiting on have been
// overtaken by the merge, so continuing spends time on stale checks.
func TestCIPollStopsWhenThePullRequestWasMerged(t *testing.T) {
	poller := &lifecyclePoller{result: providers.PullRequestPollResult{
		State:      "closed",
		Merged:     true,
		CheckState: providers.CheckStatePending,
	}}
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = func(context.Context, time.Duration) error {
		t.Fatal("ci-poll slept on a merged pull request instead of stopping")
		return nil
	}

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if poller.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", poller.calls)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success: the merge is not a failure", result.Status)
	}
	if got := result.Outputs[OutputCIStatus]; got != CIStatusMerged {
		t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, got, CIStatusMerged)
	}
}

// TestCIPollKeepsPollingAnOpenPullRequest is the guard on the narrowing: the
// ordinary pending-checks case must be untouched, or this change would turn
// every slow CI queue into an immediate stop.
func TestCIPollKeepsPollingAnOpenPullRequest(t *testing.T) {
	poller := &sequencedPoller{steps: []pollStep{
		{state: providers.CheckStatePending},
		{state: providers.CheckStatePending},
		{state: providers.CheckStatePassing},
	}}
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	slept := 0
	exec.Sleep = func(context.Context, time.Duration) error {
		slept++
		return nil
	}

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs[OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("outputs[%s] = %v, want passing", OutputCIStatus, result.Outputs[OutputCIStatus])
	}
	if slept == 0 {
		t.Fatal("ci-poll never slept; an open pull request with pending checks must keep polling")
	}
}

// TestCIPollLifecycleOutcomeIsProviderNeutral pins the decision table at the
// unit the two dispatch paths share, including the empty State a provider that
// does not report lifecycle leaves behind — which must keep polling rather
// than being read as closed.
func TestCIPollLifecycleOutcomeIsProviderNeutral(t *testing.T) {
	tests := []struct {
		name     string
		result   providers.PullRequestPollResult
		wantStop bool
		wantCI   string
	}{
		{name: "open", result: providers.PullRequestPollResult{State: "open"}, wantStop: false},
		{name: "unreported lifecycle", result: providers.PullRequestPollResult{}, wantStop: false},
		{
			name:     "closed unmerged",
			result:   providers.PullRequestPollResult{State: "closed"},
			wantStop: true, wantCI: CIStatusClosed,
		},
		{
			// ADO reports "abandoned" through adoPullRequestState, which maps
			// to the provider-neutral "closed" this executor reads.
			name:     "closed with different case",
			result:   providers.PullRequestPollResult{State: "Closed"},
			wantStop: true, wantCI: CIStatusClosed,
		},
		{
			// GitHub's spelling: closed with Merged true.
			name:     "merged",
			result:   providers.PullRequestPollResult{State: "closed", Merged: true},
			wantStop: true, wantCI: CIStatusMerged,
		},
		{
			// ADO's spelling: adoPullRequestState maps "completed" to the
			// neutral state "merged". Reading only Merged, or only the state
			// string, would leave one provider polling a landed PR.
			name:     "ado neutral merged state",
			result:   providers.PullRequestPollResult{State: "merged"},
			wantStop: true, wantCI: CIStatusMerged,
		},
		{
			// Merged wins over the state string: a provider reporting a merged
			// PR as still open must not be read as closed-without-merge.
			name:     "merged but reported open",
			result:   providers.PullRequestPollResult{State: "open", Merged: true},
			wantStop: true, wantCI: CIStatusMerged,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, stop := ciPollLifecycleOutcome(tt.result, "7")
			if stop != tt.wantStop {
				t.Fatalf("stop = %v, want %v", stop, tt.wantStop)
			}
			if !stop {
				return
			}
			if got := outcome.Outputs[OutputCIStatus]; got != tt.wantCI {
				t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, got, tt.wantCI)
			}
		})
	}
}
