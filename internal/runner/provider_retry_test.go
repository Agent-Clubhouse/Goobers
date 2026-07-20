package runner

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

type terminalCIPoller struct {
	err   error
	calls int
}

func (p *terminalCIPoller) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	p.calls++
	return providers.PullRequestPollResult{}, p.err
}

func TestRunnerDoesNotPolicyRetryTerminalCIPollProviderFailure(t *testing.T) {
	machine := ciPollRetryFixtureMachine(t, 3)
	cause := errors.New("GET /pulls/9 failed: status 401: bad credentials")
	poller := &terminalCIPoller{err: cause}
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		ciPoll, err := executor.NewCIPollExecutor(poller, rec)
		if err != nil {
			return nil, err
		}
		return executor.NewTaskExecutor(&executor.ShellExecutor{}, ciPoll)
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-terminal-ci-provider-error",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted", res.Phase)
	}
	if poller.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 despite task retry policy", poller.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-terminal-ci-provider-error"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var starts int
	var failure *journal.ErrorDetail
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			starts++
		}
		if event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultFailure) {
			failure = event.Error
		}
	}
	if starts != 1 {
		t.Fatalf("stage.started events = %d, want 1", starts)
	}
	if failure == nil || failure.Code != "poll_provider_error" || !strings.Contains(failure.Message, cause.Error()) {
		t.Fatalf("stage failure = %+v, want terminal provider cause", failure)
	}
}

func ciPollRetryFixtureMachine(t *testing.T, maxAttempts int32) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "poll",
		Tasks: []apiv1.Task{{
			Name: "poll",
			Type: apiv1.TaskDeterministic,
			Goal: "poll CI",
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
			Inputs: map[string]string{
				executor.InputKind:     executor.KindCIPoll,
				executor.InputPRNumber: "9",
			},
			Capabilities: []string{string(capability.GitHubPRWrite)},
			Retry:        &apiv1.RetryPolicy{MaxAttempts: maxAttempts},
			Next:         "gate",
		}},
		Gates: []apiv1.Gate{{
			Name:      "gate",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals"},
			Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": workflow.TargetAbort},
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "ci-poll-retry-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile ci-poll retry fixture: %v", err)
	}
	return machine
}
