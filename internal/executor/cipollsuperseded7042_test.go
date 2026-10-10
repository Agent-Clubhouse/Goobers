package executor

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// scriptedResultPoller replays one full poll result per call and stays on the
// last one once exhausted.
type scriptedResultPoller struct {
	results []providers.PullRequestPollResult
	calls   int
}

func (p *scriptedResultPoller) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	result := p.results[min(p.calls, len(p.results)-1)]
	p.calls++
	return result, nil
}

func pollResult(head string, state providers.CheckState, checks ...providers.CheckDetail) providers.PullRequestPollResult {
	return providers.PullRequestPollResult{HeadSHA: head, CheckState: state, Checks: checks}
}

func cancelledCheck(name string) providers.CheckDetail {
	return providers.CheckDetail{Name: name, State: providers.CheckStateFailing, Conclusion: "cancelled"}
}

// TestCIPollExecutor_SupersededCancelledRunIsNotAFailure covers #7042: a push
// that supersedes the polled head cancels the old head's CI, and that
// cancellation alone must not become ci-poll's terminal failing verdict.
func TestCIPollExecutor_SupersededCancelledRunIsNotAFailure(t *testing.T) {
	pending := providers.CheckDetail{Name: "test", State: providers.CheckStatePending}
	passing := providers.CheckDetail{Name: "test", State: providers.CheckStatePassing, Conclusion: "success"}
	failed := providers.CheckDetail{Name: "lint", State: providers.CheckStateFailing, Conclusion: "failure"}
	tests := []struct {
		name      string
		results   []providers.PullRequestPollResult
		wantState providers.CheckState
		wantPolls int
	}{
		{
			name: "cancelled old head then new head passes",
			results: []providers.PullRequestPollResult{
				pollResult("old", providers.CheckStateFailing, cancelledCheck("test")),
				pollResult("new", providers.CheckStatePending, pending),
				pollResult("new", providers.CheckStatePassing, passing),
			},
			wantState: providers.CheckStatePassing,
			wantPolls: 3,
		},
		{
			name: "cancelled old head then cancelled-only on new head is held again",
			results: []providers.PullRequestPollResult{
				pollResult("old", providers.CheckStateFailing, cancelledCheck("test")),
				pollResult("new", providers.CheckStateFailing, cancelledCheck("test")),
				pollResult("new", providers.CheckStatePassing, passing),
			},
			wantState: providers.CheckStatePassing,
			wantPolls: 3,
		},
		{
			name: "newer run registers on the same head and passes",
			results: []providers.PullRequestPollResult{
				pollResult("head", providers.CheckStateFailing, cancelledCheck("test")),
				pollResult("head", providers.CheckStatePassing, passing),
			},
			wantState: providers.CheckStatePassing,
			wantPolls: 2,
		},
		{
			name: "cancellation persisting on an unchanged head fails",
			results: []providers.PullRequestPollResult{
				pollResult("head", providers.CheckStateFailing, cancelledCheck("test")),
				pollResult("head", providers.CheckStateFailing, cancelledCheck("test")),
			},
			wantState: providers.CheckStateFailing,
			wantPolls: 2,
		},
		{
			name: "pending between cancellations resets confirmation",
			results: []providers.PullRequestPollResult{
				pollResult("head", providers.CheckStateFailing, cancelledCheck("test")),
				pollResult("head", providers.CheckStatePending, pending),
				pollResult("head", providers.CheckStateFailing, cancelledCheck("test")),
				pollResult("head", providers.CheckStateFailing, cancelledCheck("test")),
			},
			wantState: providers.CheckStateFailing,
			wantPolls: 4,
		},
		{
			name: "a real failure beside a cancellation fails at once",
			results: []providers.PullRequestPollResult{
				pollResult("head", providers.CheckStateFailing, cancelledCheck("test"), failed),
			},
			wantState: providers.CheckStateFailing,
			wantPolls: 1,
		},
		{
			name: "a failure without check detail fails at once",
			results: []providers.PullRequestPollResult{
				pollResult("head", providers.CheckStateFailing),
			},
			wantState: providers.CheckStateFailing,
			wantPolls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			poller := &scriptedResultPoller{results: tt.results}
			exec, err := NewCIPollExecutor(poller, newFakeRecorder())
			if err != nil {
				t.Fatal(err)
			}
			exec.Sleep = noSleep

			result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := result.Outputs[OutputCIStatus]; got != string(tt.wantState) {
				t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, got, tt.wantState)
			}
			if poller.calls != tt.wantPolls {
				t.Fatalf("poll calls = %d, want %d", poller.calls, tt.wantPolls)
			}
		})
	}
}

// TestCIPollExecutor_SupersededCancelledRunIsNotRerun pins that the held poll
// does not spend a mechanical rerun (#4750) on the superseded head.
func TestCIPollExecutor_SupersededCancelledRunIsNotRerun(t *testing.T) {
	poller := &rerunningPoller{
		fakePoller: fakePoller{
			results: []providers.CheckState{providers.CheckStateFailing, providers.CheckStatePassing},
			checks:  []providers.CheckDetail{cancelledCheck("test")},
		},
		headSHA: "head",
	}
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep
	cfg := cfgFor("o", "r", "42")
	cfg.RetryFailedChecksMaxAttempts = 1

	result, err := exec.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := result.Outputs[OutputCIStatus]; got != string(providers.CheckStatePassing) {
		t.Fatalf("outputs[%s] = %v, want passing", OutputCIStatus, got)
	}
	if poller.rerunCalls != 0 {
		t.Fatalf("reruns = %d, want 0 for a held cancellation", poller.rerunCalls)
	}
}
