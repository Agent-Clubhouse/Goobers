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

func cfgFor(owner, repo, pullID string) CIPollConfig {
	return CIPollConfig{Owner: owner, Repo: repo, PullID: pullID}
}

func TestCIPollExecutor_Pass(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePassing}}
	exec, err := NewCIPollExecutor(poller)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (the poll itself succeeded)", result.Status)
	}
	if result.Outputs[OutputCIStatus] != string(apiv1.ResultSuccess) {
		t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, result.Outputs[OutputCIStatus], apiv1.ResultSuccess)
	}
	if poller.calls != 0 {
		t.Fatalf("expected exactly one poll call, got %d", poller.calls+1)
	}
}

func TestCIPollExecutor_Fail(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStateFailing}}
	exec, err := NewCIPollExecutor(poller)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The poll itself succeeded (it determined a terminal state) even though
	// the state it determined is "CI failed" — that verdict rides in Outputs.
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if result.Outputs[OutputCIStatus] != string(apiv1.ResultFailure) {
		t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, result.Outputs[OutputCIStatus], apiv1.ResultFailure)
	}
}

func TestCIPollExecutor_PendingThenPass(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{
		providers.CheckStatePending, providers.CheckStatePending, providers.CheckStatePassing,
	}}
	exec, err := NewCIPollExecutor(poller)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep
	exec.Timeout = time.Hour // won't be hit

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs[OutputCIStatus] != string(apiv1.ResultSuccess) {
		t.Fatalf("outputs[%s] = %v, want success", OutputCIStatus, result.Outputs[OutputCIStatus])
	}
	if poller.calls != 2 {
		t.Fatalf("expected 3 poll calls (2 pending + 1 terminal), got %d", poller.calls+1)
	}
}

func TestCIPollExecutor_TimesOutIsAFailure(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePending}}
	exec, err := NewCIPollExecutor(poller)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep
	exec.Timeout = time.Minute

	base := time.Now()
	tick := 0
	exec.Now = func() time.Time {
		tm := base.Add(time.Duration(tick) * time.Minute)
		tick++
		return tm
	}

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure (the poll itself did not complete)", result.Status)
	}
	if result.Error == nil || result.Error.Code != "poll_timeout" || !result.Error.Retryable {
		t.Fatalf("error = %+v, want poll_timeout, retryable", result.Error)
	}
	if _, ok := result.Outputs[OutputCIStatus]; ok {
		t.Fatalf("outputs = %+v, want no ciStatus set on a timeout (not a claimed pass/fail)", result.Outputs)
	}
}

func TestCIPollConfigFromEnvelope_MissingLocatorIsError(t *testing.T) {
	if _, err := CIPollConfigFromEnvelope(apiv1.InvocationEnvelope{}); err == nil {
		t.Fatal("expected an error when owner/repo/pullId cannot be determined")
	}
}

func TestCIPollConfigFromEnvelope_DefaultsFromRepoRef(t *testing.T) {
	env := apiv1.InvocationEnvelope{
		RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Inputs:  map[string]interface{}{InputPRNumber: "7"},
	}
	cfg, err := CIPollConfigFromEnvelope(env)
	if err != nil {
		t.Fatalf("CIPollConfigFromEnvelope: %v", err)
	}
	if cfg.Owner != "acme" || cfg.Repo != "widgets" || cfg.PullID != "7" {
		t.Fatalf("cfg = %+v, unexpected", cfg)
	}
}

func TestNewCIPollExecutor_RequiresPoller(t *testing.T) {
	if _, err := NewCIPollExecutor(nil); err == nil {
		t.Fatal("expected error for nil poller")
	}
}
