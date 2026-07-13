package executor

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

type fakePoller struct {
	results []providers.CheckState
	calls   int
}

func (f *fakePoller) PollPullRequest(ctx context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	state := f.results[f.calls]
	if f.calls < len(f.results)-1 {
		f.calls++
	}
	return providers.PullRequestPollResult{CheckState: state}, nil
}

func noSleep(context.Context, time.Duration) error { return nil }

func gateFor(owner, repo, pullID string) apiv1.AutomatedGate {
	return apiv1.AutomatedGate{Check: CICheckName, Params: map[string]string{"owner": owner, "repo": repo, "pullId": pullID}}
}

func TestCIPollEvaluator_Pass(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePassing}}
	eval, err := NewCIPollEvaluator(poller)
	if err != nil {
		t.Fatal(err)
	}
	eval.Sleep = noSleep

	outcome, err := eval.Evaluate(context.Background(), gateFor("o", "r", "42"), apiv1.InvocationEnvelope{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if outcome != "pass" {
		t.Fatalf("outcome = %q, want pass", outcome)
	}
	if poller.calls != 0 {
		t.Fatalf("expected exactly one poll call, got %d", poller.calls+1)
	}
}

func TestCIPollEvaluator_Fail(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStateFailing}}
	eval, err := NewCIPollEvaluator(poller)
	if err != nil {
		t.Fatal(err)
	}
	eval.Sleep = noSleep

	outcome, err := eval.Evaluate(context.Background(), gateFor("o", "r", "42"), apiv1.InvocationEnvelope{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if outcome != "fail" {
		t.Fatalf("outcome = %q, want fail", outcome)
	}
}

func TestCIPollEvaluator_PendingThenPass(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{
		providers.CheckStatePending, providers.CheckStatePending, providers.CheckStatePassing,
	}}
	eval, err := NewCIPollEvaluator(poller)
	if err != nil {
		t.Fatal(err)
	}
	eval.Sleep = noSleep
	eval.Timeout = time.Hour // won't be hit

	outcome, err := eval.Evaluate(context.Background(), gateFor("o", "r", "42"), apiv1.InvocationEnvelope{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if outcome != "pass" {
		t.Fatalf("outcome = %q, want pass", outcome)
	}
	if poller.calls != 2 {
		t.Fatalf("expected 3 poll calls (2 pending + 1 terminal), got %d", poller.calls+1)
	}
}

func TestCIPollEvaluator_TimesOutWithoutClaimingFail(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePending}}
	eval, err := NewCIPollEvaluator(poller)
	if err != nil {
		t.Fatal(err)
	}
	eval.Sleep = noSleep
	eval.Timeout = time.Minute

	base := time.Now()
	tick := 0
	eval.Now = func() time.Time {
		t := base.Add(time.Duration(tick) * time.Minute)
		tick++
		return t
	}

	outcome, err := eval.Evaluate(context.Background(), gateFor("o", "r", "42"), apiv1.InvocationEnvelope{})
	if err == nil {
		t.Fatalf("expected a timeout error, got outcome %q", outcome)
	}
	if outcome != "" {
		t.Fatalf("outcome on timeout = %q, want empty (not a claimed fail/pass)", outcome)
	}
}

func TestCIPollEvaluator_WrongCheckName(t *testing.T) {
	eval, err := NewCIPollEvaluator(&fakePoller{results: []providers.CheckState{providers.CheckStatePassing}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eval.Evaluate(context.Background(), apiv1.AutomatedGate{Check: "something-else"}, apiv1.InvocationEnvelope{})
	if err == nil {
		t.Fatal("expected an error for a non-ci-poll check name")
	}
}

func TestCIPollEvaluator_MissingLocatorIsError(t *testing.T) {
	eval, err := NewCIPollEvaluator(&fakePoller{results: []providers.CheckState{providers.CheckStatePassing}})
	if err != nil {
		t.Fatal(err)
	}
	gate := apiv1.AutomatedGate{Check: CICheckName}
	_, err = eval.Evaluate(context.Background(), gate, apiv1.InvocationEnvelope{})
	if err == nil {
		t.Fatal("expected an error when owner/repo/pullId cannot be determined")
	}
}

func TestCIPollEvaluator_LocatorDefaultsFromRepoRef(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePassing}}
	eval, err := NewCIPollEvaluator(poller)
	if err != nil {
		t.Fatal(err)
	}
	gate := apiv1.AutomatedGate{Check: CICheckName, Params: map[string]string{"pullId": "7"}}
	env := apiv1.InvocationEnvelope{RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"}}

	outcome, err := eval.Evaluate(context.Background(), gate, env)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if outcome != "pass" {
		t.Fatalf("outcome = %q, want pass", outcome)
	}
}

func TestNewCIPollEvaluator_RequiresPoller(t *testing.T) {
	if _, err := NewCIPollEvaluator(nil); err == nil {
		t.Fatal("expected error for nil poller")
	}
}
