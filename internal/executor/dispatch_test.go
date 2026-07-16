package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/providers"
)

func TestTaskExecutor_DefaultsToShell(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	te, err := NewTaskExecutor(shell, nil)
	if err != nil {
		t.Fatal(err)
	}

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
	ciPoll, err := NewCIPollExecutor(poller)
	if err != nil {
		t.Fatal(err)
	}
	ciPoll.Sleep = noSleep
	te, err := NewTaskExecutor(shell, ciPoll)
	if err != nil {
		t.Fatal(err)
	}

	env := apiv1.InvocationEnvelope{
		TaskID:  "t1",
		RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Inputs:  map[string]interface{}{InputKind: KindCIPoll, InputPRNumber: "9"},
	}
	result, err := te.Run(context.Background(), env, apiv1.DeterministicRun{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs[OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("outputs = %+v, want ciStatus=%q", result.Outputs, providers.CheckStatePassing)
	}
}

func TestTaskExecutor_CIPollWithoutConfiguredExecutorFailsClosed(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	te, err := NewTaskExecutor(shell, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKind: KindCIPoll}}
	if _, err := te.Run(context.Background(), env, apiv1.DeterministicRun{}); err == nil {
		t.Fatal("expected an error when kind=ci-poll is declared but no CIPollExecutor is configured")
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
			ciPoll, err := NewCIPollExecutor(poller)
			if err != nil {
				t.Fatal(err)
			}
			ciPoll.MaxConsecutivePollErrors = 1
			ciPoll.Sleep = noSleep
			te, err := NewTaskExecutor(shell, ciPoll)
			if err != nil {
				t.Fatal(err)
			}

			env := apiv1.InvocationEnvelope{
				TaskID:  "poll",
				RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
				Inputs:  map[string]interface{}{InputKind: KindCIPoll, InputPRNumber: "9"},
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

func TestTaskExecutor_UnknownKindIsError(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	te, err := NewTaskExecutor(shell, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKind: "something-else"}}
	if _, err := te.Run(context.Background(), env, apiv1.DeterministicRun{}); err == nil {
		t.Fatal("expected an error for an unknown kind")
	}
}

func TestNewTaskExecutor_RequiresShell(t *testing.T) {
	if _, err := NewTaskExecutor(nil, nil); err == nil {
		t.Fatal("expected an error for a nil shell executor")
	}
}
