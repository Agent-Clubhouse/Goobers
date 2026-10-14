package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/providers"
)

// rerunScriptedPoller is a scriptedResultPoller that can rerun checks.
type rerunScriptedPoller struct {
	scriptedResultPoller
	rerunCalls int
	rerunErr   error
}

func (p *rerunScriptedPoller) RerunFailedChecks(context.Context, providers.RepositoryRef, string) error {
	p.rerunCalls++
	return p.rerunErr
}

func noVerdictCheck(name string) providers.CheckDetail {
	return providers.CheckDetail{Name: name, State: providers.CheckStatePending, Conclusion: "cancelled", NoVerdict: true}
}

// outagePoll is a head whose every check a CI outage cancelled (#6817).
func outagePoll(head string) providers.PullRequestPollResult {
	return pollResult(head, providers.CheckStatePending, noVerdictCheck("build"), noVerdictCheck("test"))
}

func runNoVerdictPoll(t *testing.T, poller PRPoller, retries int) (map[string]interface{}, error) {
	t.Helper()
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep
	cfg := cfgFor("o", "r", "42")
	cfg.RetryFailedChecksMaxAttempts = retries
	result, err := exec.Run(context.Background(), cfg)
	return result.Outputs, err
}

// TestCIPollExecutor_NoVerdictHeadEndsAsInfrastructure is #6817's bound: a
// head whose CI finished without a verdict will not change by itself, so
// ci-poll must not wait out its timeout (and the workflow's timeout re-polls)
// as if CI were running. Once two consecutive polls of the same head confirm
// it, and no rerun is possible, the poll ends with the ci_no_verdict
// infrastructure error: never a failing verdict, never a work failure.
func TestCIPollExecutor_NoVerdictHeadEndsAsInfrastructure(t *testing.T) {
	poller := &scriptedResultPoller{results: []providers.PullRequestPollResult{outagePoll("head"), outagePoll("head")}}
	_, err := runNoVerdictPoll(t, poller, 0)
	if !IsCIPollInfrastructureError(err) {
		t.Fatalf("err = %v, want a ci-poll infrastructure error", err)
	}
	if code := CIPollFailureCode(err); code != CIPollNoVerdictCode {
		t.Fatalf("failure code = %q, want %q", code, CIPollNoVerdictCode)
	}
	if !strings.Contains(err.Error(), "build, test") || !strings.Contains(err.Error(), "without a verdict") {
		t.Fatalf("err = %v, want it to name the no-verdict checks", err)
	}
	if poller.calls != 2 {
		t.Fatalf("polls = %d, want 2 (one sighting, one confirmation)", poller.calls)
	}
}

// TestCIPollExecutor_NoVerdictNeedsConfirmation pins the #7042 hold: a first
// no-verdict sighting may be a superseded head whose replacement run has not
// registered yet, so the poll keeps waiting when the next poll differs.
func TestCIPollExecutor_NoVerdictNeedsConfirmation(t *testing.T) {
	running := providers.CheckDetail{Name: "build", State: providers.CheckStatePending}
	passing := providers.CheckDetail{Name: "build", State: providers.CheckStatePassing, Conclusion: "success"}
	for name, results := range map[string][]providers.PullRequestPollResult{
		"head moved": {
			outagePoll("old"),
			outagePoll("new"),
			pollResult("new", providers.CheckStatePassing, passing),
		},
		"a newer run registered": {
			outagePoll("head"),
			pollResult("head", providers.CheckStatePending, running),
			outagePoll("head"),
			pollResult("head", providers.CheckStatePassing, passing),
		},
		"a running check beside a cancelled one is still running": {
			pollResult("head", providers.CheckStatePending, noVerdictCheck("lint"), running),
			pollResult("head", providers.CheckStatePending, noVerdictCheck("lint"), running),
			pollResult("head", providers.CheckStatePassing, passing),
		},
	} {
		t.Run(name, func(t *testing.T) {
			poller := &scriptedResultPoller{results: results}
			outputs, err := runNoVerdictPoll(t, poller, 0)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if outputs[OutputCIStatus] != string(providers.CheckStatePassing) {
				t.Fatalf("outputs[%s] = %v, want passing", OutputCIStatus, outputs[OutputCIStatus])
			}
		})
	}
}

// TestCIPollExecutor_NoVerdictHeadIsRerun pins the opt-in rerun: a workflow
// that sets retryFailedChecksMaxAttempts (pr-remediation's retry-failed-ci)
// reruns a confirmed no-verdict head, polls the rerun, and only once its
// attempts are spent ends as infrastructure.
func TestCIPollExecutor_NoVerdictHeadIsRerun(t *testing.T) {
	running := providers.CheckDetail{Name: "build", State: providers.CheckStatePending}
	passing := providers.CheckDetail{Name: "build", State: providers.CheckStatePassing, Conclusion: "success"}

	t.Run("rerun passes", func(t *testing.T) {
		poller := &rerunScriptedPoller{scriptedResultPoller: scriptedResultPoller{results: []providers.PullRequestPollResult{
			outagePoll("head"), outagePoll("head"),
			pollResult("head", providers.CheckStatePending, running),
			pollResult("head", providers.CheckStatePassing, passing),
		}}}
		outputs, err := runNoVerdictPoll(t, poller, 1)
		if err != nil || outputs[OutputCIStatus] != string(providers.CheckStatePassing) {
			t.Fatalf("outputs = %v, err = %v; want passing after the rerun", outputs, err)
		}
		if poller.rerunCalls != 1 {
			t.Fatalf("reruns = %d, want 1", poller.rerunCalls)
		}
	})

	t.Run("rerun cancelled again", func(t *testing.T) {
		poller := &rerunScriptedPoller{scriptedResultPoller: scriptedResultPoller{results: []providers.PullRequestPollResult{
			outagePoll("head"), outagePoll("head"),
			pollResult("head", providers.CheckStatePending, running),
			outagePoll("head"), outagePoll("head"),
		}}}
		_, err := runNoVerdictPoll(t, poller, 1)
		if CIPollFailureCode(err) != CIPollNoVerdictCode || !strings.Contains(err.Error(), "after 1 rerun(s)") {
			t.Fatalf("err = %v, want ci_no_verdict after one rerun", err)
		}
		if poller.rerunCalls != 1 {
			t.Fatalf("reruns = %d, want 1 (bounded by retryFailedChecksMaxAttempts)", poller.rerunCalls)
		}
	})

	t.Run("rerun refused", func(t *testing.T) {
		refused := errors.New("credential lacks the GitHub Actions write permission")
		poller := &rerunScriptedPoller{
			scriptedResultPoller: scriptedResultPoller{results: []providers.PullRequestPollResult{outagePoll("head"), outagePoll("head")}},
			rerunErr:             refused,
		}
		_, err := runNoVerdictPoll(t, poller, 1)
		if CIPollFailureCode(err) != CIPollNoVerdictCode || !errors.Is(err, refused) {
			t.Fatalf("err = %v, want ci_no_verdict carrying the refused rerun", err)
		}
		if poller.rerunCalls != 1 {
			t.Fatalf("reruns = %d, want 1", poller.rerunCalls)
		}
	})
}

// TestCIPollKindExecutor_NoVerdictIsInfrastructureFailure pins the dispatch
// seam: the no-verdict error reaches the runner as an infrastructure failure
// (retried within the infrastructure budget, exempt from the failure-streak
// park) carrying the ci_no_verdict code, not as a failing ciStatus.
func TestCIPollKindExecutor_NoVerdictIsInfrastructureFailure(t *testing.T) {
	err := error(&ciPollInfrastructureError{cause: &ciPollNoVerdictError{head: "head", checks: []string{"build"}}})
	if !IsCIPollInfrastructureError(err) || CIPollFailureCode(err) != CIPollNoVerdictCode {
		t.Fatalf("classification of %v: infra=%v code=%q", err, IsCIPollInfrastructureError(err), CIPollFailureCode(err))
	}
	if !invoke.IsInfrastructureFailure(invoke.InfrastructureFailure(StageFailure(CIPollFailureCode(err), err))) {
		t.Fatal("want the dispatch wrapping to carry the infrastructure marker")
	}
}
