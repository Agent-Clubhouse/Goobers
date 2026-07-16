package executor

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestShellExecutor_TypedErrorCodeLiftedFromResultFile is #614's executor-side
// acceptance: a command that exits nonzero after writing its declared result
// file with the OutputErrorCode convention gets that code as the stage's
// ErrorInfo — not nonzero_exit, and (because the file exists) not the
// missing_result_file that used to bury a GitHub rate-limit 403.
func TestShellExecutor_TypedErrorCodeLiftedFromResultFile(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "claimed-item.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo '{"errorCode":"github_rate_limited","errorMessage":"list work items: github rate limited, resets at 2026-07-16T16:59:10Z","errorRetryable":true,"rateLimitReset":"2026-07-16T16:59:10Z"}' > claimed-item.json; exit 1`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "github_rate_limited" {
		t.Fatalf("error = %+v, want code github_rate_limited", result.Error)
	}
	if !result.Error.Retryable {
		t.Fatalf("error = %+v, want retryable (quota resets on the clock)", result.Error)
	}
	if result.Error.Message != "list work items: github rate limited, resets at 2026-07-16T16:59:10Z" {
		t.Fatalf("error message = %q, want the stage's own structured message", result.Error.Message)
	}
	// The structured context (reset time) still reaches the journaled outputs.
	if result.Outputs["rateLimitReset"] != "2026-07-16T16:59:10Z" {
		t.Fatalf("outputs[rateLimitReset] = %v, want the reset timestamp", result.Outputs["rateLimitReset"])
	}
}

// TestShellExecutor_ErrorCodeIgnoredOnZeroExit is the negative control:
// OutputErrorCode is only ever consulted on a nonzero exit — a command that
// exits 0 stays a success no matter what its result file says, so a stale or
// echoed errorCode field can never fail a healthy stage.
func TestShellExecutor_ErrorCodeIgnoredOnZeroExit(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "claimed-item.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo '{"errorCode":"github_rate_limited"}' > claimed-item.json`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (errorCode must be inert on exit 0)", result.Status)
	}
	if result.Error != nil {
		t.Fatalf("error = %+v, want nil", result.Error)
	}
}

// TestShellExecutor_NonzeroExitWithoutErrorCodeStaysGeneric pins the default:
// a plain failure whose result file carries no OutputErrorCode keeps the
// pre-#614 nonzero_exit code exactly.
func TestShellExecutor_NonzeroExitWithoutErrorCodeStaysGeneric(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "claimed-item.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo '{"claimed":false}' > claimed-item.json; exit 3`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "nonzero_exit" {
		t.Fatalf("error = %+v, want nonzero_exit", result.Error)
	}
}
