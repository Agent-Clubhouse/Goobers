package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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

// retryFailedCIEnvelope builds the invocation the shipped pr-remediation
// workflow gives its retry-failed-ci stage: the stage's own declared inputs,
// plus the PR number and the carried outputs its inputsFrom supplies.
func retryFailedCIEnvelope(t *testing.T) (apiv1.InvocationEnvelope, apiv1.Task) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "pr-remediation.yaml"))
	if err != nil {
		t.Fatalf("read pr-remediation.yaml: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal pr-remediation.yaml: %v", err)
	}
	for _, task := range w.Spec.Tasks {
		if task.Name != "retry-failed-ci" {
			continue
		}
		inputs := map[string]interface{}{}
		for key, value := range task.Inputs {
			inputs[key] = value
		}
		for key := range task.InputsFrom {
			inputs[key] = "carried-" + key
		}
		inputs[InputPRNumber] = "42"
		return apiv1.InvocationEnvelope{RepoRef: apiv1.RepoRef{Owner: "o", Name: "r"}, Inputs: inputs}, task
	}
	t.Fatal("pr-remediation declares no retry-failed-ci stage")
	return apiv1.InvocationEnvelope{}, apiv1.Task{}
}

// TestShippedRetryFailedCIContinuesOnNoVerdictHead is the #6817 re-review
// acceptance: a needs-remediation PR whose head has every check cancelled
// reaches remediation-checkpoint. pr-remediation's retry-failed-ci reruns the
// head once and, when the rerun is cancelled too, reports a determined
// pending result with its carried outputs, so the run goes on to
// remediation-checkpoint (the stage's next) and fixes the PR's other causes.
// Failing the stage instead would end every cycle before the checkpoint,
// uncharged, and reselect the same PR forever.
func TestShippedRetryFailedCIContinuesOnNoVerdictHead(t *testing.T) {
	env, task := retryFailedCIEnvelope(t)
	if task.Next != "remediation-checkpoint" {
		t.Fatalf("retry-failed-ci next = %q, want remediation-checkpoint", task.Next)
	}
	cfg, err := CIPollConfigFromEnvelope(env)
	if err != nil {
		t.Fatalf("CIPollConfigFromEnvelope: %v", err)
	}
	startupFailed := providers.CheckDetail{Name: "workflow", State: providers.CheckStatePending, Conclusion: "startup_failure", NoVerdict: true}
	running := providers.CheckDetail{Name: "build", State: providers.CheckStatePending}
	poller := &rerunScriptedPoller{scriptedResultPoller: scriptedResultPoller{results: []providers.PullRequestPollResult{
		outagePoll("head"), outagePoll("head"),
		pollResult("head", providers.CheckStatePending, running),
		pollResult("head", providers.CheckStatePending, noVerdictCheck("build"), startupFailed),
		pollResult("head", providers.CheckStatePending, noVerdictCheck("build"), startupFailed),
	}}}
	recorder := newFakeRecorder()
	exec, err := NewCIPollExecutor(poller, recorder)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep

	result, err := exec.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v, want a determined result so the run reaches remediation-checkpoint", err)
	}
	if result.Status != apiv1.ResultSuccess || result.Outputs[OutputCIStatus] != string(providers.CheckStatePending) {
		t.Fatalf("result = %+v, want success with ciStatus=pending", result)
	}
	if poller.rerunCalls != 1 {
		t.Fatalf("reruns = %d, want the one the stage allows", poller.rerunCalls)
	}
	for _, carried := range []string{"selectedNumber", "remediationCauses", "attemptedHeadSha"} {
		if result.Outputs[carried] != "carried-"+carried {
			t.Errorf("outputs[%s] = %v, want it carried to remediation-checkpoint", carried, result.Outputs[carried])
		}
	}
	if result.Outputs[OutputCINoVerdictChecks] != "build,workflow" {
		t.Errorf("outputs[%s] = %v, want build,workflow", OutputCINoVerdictChecks, result.Outputs[OutputCINoVerdictChecks])
	}
	// The startup failure that survived its rerun may be the PR's own
	// workflow error: it must reach the agent as evidence, not be dropped.
	if len(result.Artifacts) != 1 || !strings.Contains(string(recorder.recorded[CIChecksArtifactName]), `"conclusion":"startup_failure"`) {
		t.Fatalf("artifacts = %+v, evidence = %s; want ci-checks.json carrying the startup_failure check", result.Artifacts, recorder.recorded[CIChecksArtifactName])
	}
}

func TestCIPollConfigFromEnvelope_NoVerdictMode(t *testing.T) {
	for value, want := range map[string]bool{"": false, "fail": false, "continue": true} {
		env := apiv1.InvocationEnvelope{RepoRef: apiv1.RepoRef{Owner: "o", Name: "r"}, Inputs: map[string]interface{}{InputPRNumber: "7"}}
		if value != "" {
			env.Inputs[InputNoVerdict] = value
		}
		cfg, err := CIPollConfigFromEnvelope(env)
		if err != nil || cfg.NoVerdictContinue != want {
			t.Errorf("noVerdict %q: continue = %v, err = %v; want %v", value, cfg.NoVerdictContinue, err, want)
		}
	}
	env := apiv1.InvocationEnvelope{RepoRef: apiv1.RepoRef{Owner: "o", Name: "r"}, Inputs: map[string]interface{}{InputPRNumber: "7", InputNoVerdict: "wait"}}
	if _, err := CIPollConfigFromEnvelope(env); err == nil {
		t.Fatal("want an error for an unknown noVerdict mode")
	}
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
