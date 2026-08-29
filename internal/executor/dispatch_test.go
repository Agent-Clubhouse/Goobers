package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/providers"
)

func newRegisteredTaskExecutor(t *testing.T, shell *ShellExecutor, ciPoll *CIPollExecutor) *TaskExecutor {
	t.Helper()
	registry := NewKindRegistry()
	if err := registry.Register(KindShell, shell); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(KindCIPoll, NewCIPollKindExecutor(ciPoll)); err != nil {
		t.Fatal(err)
	}
	executor, err := NewTaskExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestTaskExecutor_DefaultsToShell(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	te := newRegisteredTaskExecutor(t, shell, nil)

	env := apiv1.InvocationEnvelope{TaskID: "t1", Workspace: t.TempDir()}
	result, err := te.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
}

func TestTaskExecutor_RoutesToCIPoll(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePassing}}
	ciPoll, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}

	ciPoll.Sleep = noSleep
	te := newRegisteredTaskExecutor(t, shell, ciPoll)

	env := apiv1.InvocationEnvelope{
		TaskID:       "t1",
		RepoRef:      apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Capabilities: []string{string(capability.ProviderPRWrite)},
		Inputs:       map[string]interface{}{InputKind: KindCIPoll, InputPRNumber: "9"},
	}
	result, err := te.Run(context.Background(), env, apiv1.DeterministicRun{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs[OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("outputs = %+v, want ciStatus=%q", result.Outputs, providers.CheckStatePassing)
	}
}

func TestTaskExecutor_CIPollHonorsDeclaredDurationLimit(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePending}}
	ciPoll, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	ciPoll.Sleep = noSleep
	base := time.Now()
	tick := 0
	ciPoll.Now = func() time.Time {
		at := base.Add(time.Duration(tick) * 2 * time.Second)
		tick++
		return at
	}
	te := newRegisteredTaskExecutor(t, shell, ciPoll)

	env := apiv1.InvocationEnvelope{
		RepoRef:      apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Capabilities: []string{string(capability.ProviderPRWrite)},
		Limits:       apiv1.Limits{MaxDurationSeconds: 1},
		Inputs:       map[string]interface{}{InputKind: KindCIPoll, InputPRNumber: "9"},
	}
	result, err := te.Run(context.Background(), env, apiv1.DeterministicRun{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "poll_timeout" {
		t.Fatalf("result = %+v, want typed poll_timeout failure", result)
	}
	if !strings.Contains(result.Error.Message, "after 900ms") {
		t.Fatalf("timeout message = %q, want poll budget inside declared 1s stage limit", result.Error.Message)
	}
}

func TestTaskExecutor_CIPollUsesDeclaredPollIntervalAsJitterCeiling(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	poller := &fakePoller{results: []providers.CheckState{
		providers.CheckStatePending,
		providers.CheckStatePassing,
	}}
	ciPoll, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	var slept time.Duration
	ciPoll.Sleep = func(_ context.Context, interval time.Duration) error {
		slept = interval
		return nil
	}
	te := newRegisteredTaskExecutor(t, shell, ciPoll)

	env := apiv1.InvocationEnvelope{
		RepoRef:      apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Capabilities: []string{string(capability.ProviderPRWrite)},
		Inputs: map[string]interface{}{
			InputKind:            KindCIPoll,
			InputPRNumber:        "9",
			InputPollIntervalSec: "7s",
		},
	}
	result, err := te.Run(context.Background(), env, apiv1.DeterministicRun{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if slept < 3500*time.Millisecond || slept > 7*time.Second {
		t.Fatalf("poll sleep = %s, want jittered range [3.5s, 7s]", slept)
	}
}

type dispatchCountingPoller struct {
	calls int
}

func (p *dispatchCountingPoller) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	p.calls++
	return providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}, nil
}

func TestTaskExecutor_CIPollWithoutCapabilityFailsBeforePolling(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	poller := &dispatchCountingPoller{}
	ciPoll, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	te := newRegisteredTaskExecutor(t, shell, ciPoll)

	env := apiv1.InvocationEnvelope{
		RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Inputs:  map[string]interface{}{InputKind: KindCIPoll, InputPRNumber: "9"},
	}
	_, err = te.Run(context.Background(), env, apiv1.DeterministicRun{})
	if err == nil || !strings.Contains(err.Error(), `requires declared capability "provider:pr:write"`) {
		t.Fatalf("Run error = %v, want missing-capability error", err)
	}
	if poller.calls != 0 {
		t.Fatalf("poller calls = %d, want 0", poller.calls)
	}
}

func TestTaskExecutor_CIPollWithoutConfiguredExecutorFailsClosed(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	te := newRegisteredTaskExecutor(t, shell, nil)
	env := apiv1.InvocationEnvelope{
		Capabilities: []string{string(capability.ProviderPRWrite)},
		Inputs:       map[string]interface{}{InputKind: KindCIPoll},
	}
	if _, err := te.Run(context.Background(), env, apiv1.DeterministicRun{}); err == nil || err.Error() != "executor: kind=ci-poll declared but no CIPollExecutor is configured" {
		t.Fatalf("Run error = %v, want missing CIPollExecutor error", err)
	}
}

func TestTaskExecutor_ClassifiesCIPollProviderFailures(t *testing.T) {
	cases := []struct {
		name               string
		err                error
		wantInfrastructure bool
		wantFailure        bool
	}{
		{"server error", errors.New("GET /pulls/9 failed: status 503: unavailable"), true, false},
		{"rate limit", errors.New("GET /pulls/9 failed: status 429: retry later"), true, false},
		{"guided forbidden rate limit", errors.New("GET /pulls/9 failed: status 403: Retry-After=60"), true, false},
		{"authentication", errors.New("GET /pulls/9 failed: status 401: bad credentials"), false, true},
		{"authorization", errors.New("GET /pulls/9 failed: status 403: forbidden"), false, true},
		{"deterministic request", errors.New("GET /pulls/9 failed: status 422: invalid"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shell, _ := newTestExecutor(t, nil)
			poller := &sequencedPoller{steps: []pollStep{{err: tc.err}}}
			ciPoll, err := NewCIPollExecutor(poller, newFakeRecorder())
			if err != nil {
				t.Fatal(err)
			}
			ciPoll.MaxConsecutivePollErrors = 1
			ciPoll.Sleep = noSleep
			te := newRegisteredTaskExecutor(t, shell, ciPoll)

			env := apiv1.InvocationEnvelope{
				TaskID:       "poll",
				RepoRef:      apiv1.RepoRef{Owner: "acme", Name: "widgets"},
				Capabilities: []string{string(capability.ProviderPRWrite)},
				Inputs:       map[string]interface{}{InputKind: KindCIPoll, InputPRNumber: "9"},
			}
			result, runErr := te.Run(context.Background(), env, apiv1.DeterministicRun{})
			if got := invoke.IsInfrastructureFailure(runErr); got != tc.wantInfrastructure {
				t.Fatalf("infrastructure failure = %v, want %v (err=%v)", got, tc.wantInfrastructure, runErr)
			}
			if tc.wantFailure {
				if runErr != nil {
					t.Fatalf("Run returned dispatch error %v, want failure result", runErr)
				}
				if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "poll_provider_error" || result.Error.Retryable {
					t.Fatalf("result = %+v, want non-retryable poll_provider_error failure", result)
				}
				if !strings.Contains(result.Error.Message, tc.err.Error()) {
					t.Fatalf("failure message %q does not preserve provider cause %q", result.Error.Message, tc.err)
				}
			}
		})
	}
}

type testKindExecutor struct {
	called bool
}

func (e *testKindExecutor) Run(_ context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	e.called = true
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: env.TaskID + ":" + strings.Join(run.Command, " "),
	}, nil
}

func TestTaskExecutor_RoutesToRegisteredKind(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	example := &testKindExecutor{}
	registry := NewKindRegistry()
	if err := registry.Register(KindShell, shell); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("example", example); err != nil {
		t.Fatal(err)
	}
	te, err := NewTaskExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}

	result, err := te.Run(context.Background(), apiv1.InvocationEnvelope{
		TaskID: "registered",
		Inputs: map[string]interface{}{InputKind: "example"},
	}, apiv1.DeterministicRun{Command: []string{"example", "argument"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !example.called || result.Status != apiv1.ResultSuccess || result.Summary != "registered:example argument" {
		t.Fatalf("registered executor called = %v, result = %+v", example.called, result)
	}
}

func TestKindRegistryRejectsInvalidRegistrations(t *testing.T) {
	registry := NewKindRegistry()
	example := &testKindExecutor{}
	if err := registry.Register("", example); err == nil {
		t.Fatal("expected empty kind registration to fail")
	}
	if err := registry.Register("example", nil); err == nil {
		t.Fatal("expected nil executor registration to fail")
	}
	if err := registry.Register("example", example); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("example", example); err == nil {
		t.Fatal("expected duplicate kind registration to fail")
	}
}

func TestKindRegistryRejectsTypedNilExecutor(t *testing.T) {
	registry := NewKindRegistry()
	var shell *ShellExecutor
	if err := registry.Register(KindShell, shell); err == nil || err.Error() != `executor: kind "shell" executor must not be nil` {
		t.Fatalf("Register error = %v, want typed-nil executor error", err)
	}
	if _, err := NewTaskExecutor(registry); err == nil || err.Error() != "executor: shell executor must not be nil" {
		t.Fatalf("NewTaskExecutor error = %v, want missing shell error", err)
	}
}

func TestTaskExecutor_UnknownKindIsError(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	te := newRegisteredTaskExecutor(t, shell, nil)
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKind: "something-else"}}
	if _, err := te.Run(context.Background(), env, apiv1.DeterministicRun{}); err == nil || err.Error() != "executor: unknown kind something-else" {
		t.Fatalf("Run error = %v, want equivalent unknown-kind error", err)
	}
}

func TestNewTaskExecutor_RequiresShell(t *testing.T) {
	if _, err := NewTaskExecutor(nil); err == nil {
		t.Fatal("expected nil registry to fail")
	}
	if _, err := NewTaskExecutor(NewKindRegistry()); err == nil || err.Error() != "executor: shell executor must not be nil" {
		t.Fatalf("NewTaskExecutor error = %v, want missing shell error", err)
	}

	var shell *ShellExecutor
	registry := NewKindRegistry()
	registry.executors[KindShell] = shell
	if _, err := NewTaskExecutor(registry); err == nil || err.Error() != "executor: shell executor must not be nil" {
		t.Fatalf("NewTaskExecutor error = %v, want typed-nil shell error", err)
	}
}
