package harness

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestReclassifyOperationalFailureBlock is #5262's core unit acceptance: a
// recognized operational environment failure reported as blocked becomes a
// retryable failure, so the ordinary failure route can carry it into a declared
// remediation gate instead of terminating the run at the #544 blocked terminal.
//
// Each assertion here is a property the issue names, not incidental detail: the
// producer's own code survives (a gate binds error.code as an input), the
// message survives verbatim, retryable becomes true (how the gate is told an
// infrastructure retry is legitimate), and the reclassification is recorded
// rather than silent.
func TestReclassifyOperationalFailureBlock(t *testing.T) {
	for _, code := range []string{
		"DEPENDENCY_RESTORE_FAILED",
		"dependency-restore-failed",
		"  Dependency.Restore.Failed  ",
		"DEPENDENCIES_RESTORE_FAILED",
	} {
		result := apiv1.ResultEnvelope{
			Status:  apiv1.ResultBlocked,
			Summary: "could not restore the module cache",
			Error: &apiv1.ErrorInfo{
				Code:      code,
				Message:   "go mod download: dial tcp: i/o timeout",
				Retryable: false,
			},
		}
		reclassifyOperationalFailureBlock(&result)

		if result.Status != apiv1.ResultFailure {
			t.Errorf("code %q: status = %q, want failure", code, result.Status)
		}
		// The ORIGINAL code, not a synthetic HARNESS_* one. internal/gate binds
		// error.code as a gate input, so rewriting it is what would stop a gate
		// authored against the producer's real code from branching on it.
		if result.Error.Code != code {
			t.Errorf("code %q: error.Code = %q, want the producer's code preserved", code, result.Error.Code)
		}
		if result.Error.Message != "go mod download: dial tcp: i/o timeout" {
			t.Errorf("code %q: error.Message = %q, want it preserved verbatim", code, result.Error.Message)
		}
		if !result.Error.Retryable {
			t.Errorf("code %q: retryable = false, want true so a gate can choose an infrastructure retry", code)
		}
		if got, ok := result.Outputs["operationalFailure"].(bool); !ok || !got {
			t.Errorf("code %q: outputs = %+v, want operationalFailure=true", code, result.Outputs)
		}
		if got, ok := result.Outputs["reclassifiedFromBlocked"].(bool); !ok || !got {
			t.Errorf("code %q: outputs = %+v, want reclassifiedFromBlocked=true", code, result.Outputs)
		}
		// Diagnostic retention: the agent's own narrative must still be readable.
		if !strings.Contains(result.Summary, "could not restore the module cache") {
			t.Errorf("code %q: summary = %q, want the agent's authored summary retained", code, result.Summary)
		}
	}
}

// TestReclassifyOperationalFailureLeavesGenuineBlocks is the no-regression half
// of #5262 and the reason the matcher is code-exact rather than prose- or
// substring-based. A genuine dependency block must still escalate under #544,
// and an unknown or ambiguous block must not be silently converted.
//
// DEPENDENCY_NOT_MET is the case #2197 deliberately left untouched; the
// OPTIONAL_..._BUT_CONTINUED entry is the one a substring matcher would have
// converted while meaning the opposite.
func TestReclassifyOperationalFailureLeavesGenuineBlocks(t *testing.T) {
	for _, code := range []string{
		"DEPENDENCY_NOT_MET",
		"CONTENT_EXCLUDED",
		"OPTIONAL_DEPENDENCY_RESTORE_FAILED_BUT_CONTINUED",
		"DEPENDENCY_RESTORE_FAILED_UPSTREAM_BLOCKER",
		"",
	} {
		result := apiv1.ResultEnvelope{
			Status:  apiv1.ResultBlocked,
			Summary: "waiting on #441",
			Error:   &apiv1.ErrorInfo{Code: code, Message: "issue 441 must merge first"},
			Outputs: map[string]interface{}{"blockedBy": "441"},
		}
		before := result
		reclassifyOperationalFailureBlock(&result)

		if result.Status != before.Status {
			t.Errorf("code %q: status = %q, want the genuine block left at %q", code, result.Status, before.Status)
		}
		if result.Summary != before.Summary || result.Error.Code != before.Error.Code {
			t.Errorf("code %q: genuine block was rewritten: %+v", code, result)
		}
		if result.Error.Retryable {
			t.Errorf("code %q: retryable was flipped on a genuine block", code)
		}
		if _, ok := result.Outputs["operationalFailure"]; ok {
			t.Errorf("code %q: outputs = %+v, want no reclassification marker", code, result.Outputs)
		}
	}
}

// TestReclassifyOperationalFailureIgnoresNonBlocked pins that the conversion is
// blocked-only. A producer that already reported failure or success is never
// rewritten, so this cannot change the meaning of a status the producer chose
// correctly.
func TestReclassifyOperationalFailureIgnoresNonBlocked(t *testing.T) {
	for _, status := range []apiv1.ResultStatus{apiv1.ResultSuccess, apiv1.ResultFailure} {
		result := apiv1.ResultEnvelope{
			Status: status,
			Error:  &apiv1.ErrorInfo{Code: "DEPENDENCY_RESTORE_FAILED"},
		}
		reclassifyOperationalFailureBlock(&result)
		if result.Status != status {
			t.Errorf("status %q was rewritten to %q", status, result.Status)
		}
		if _, ok := result.Outputs["operationalFailure"]; ok {
			t.Errorf("status %q: outputs = %+v, want untouched", status, result.Outputs)
		}
	}
}

// TestReclassifyOperationalFailureAbsentErrorDetail covers the #5262 acceptance
// case of a block with no error detail at all. There is no code to establish the
// classification, so the block is left exactly as authored — the ambiguous case
// must not be converted — and nothing panics on the nil.
func TestReclassifyOperationalFailureAbsentErrorDetail(t *testing.T) {
	result := apiv1.ResultEnvelope{Status: apiv1.ResultBlocked, Summary: "blocked, no detail"}
	reclassifyOperationalFailureBlock(&result)
	if result.Status != apiv1.ResultBlocked {
		t.Fatalf("status = %q, want blocked left untouched with no error detail", result.Status)
	}
	if result.Outputs != nil {
		t.Fatalf("outputs = %+v, want nil", result.Outputs)
	}
	reclassifyOperationalFailureBlock(nil)
}

// TestMissingRequiredToolsCodeIsRecognized closes the concrete gap #5262 calls
// the "unrecognized missing-tool failure". isMissingCapabilityCode matches by
// substring, which recognized MISSING_TOOLS but NOT MISSING_REQUIRED_TOOLS —
// the qualifier breaks the match — so a producer naming its missing tools the
// more natural way fell through to the #544 blocked terminal and parked every
// item the run had claimed.
//
// The paired negative list is the point of the test: closing the gap must not
// widen the matcher into codes that merely mention a tool or a requirement.
func TestMissingRequiredToolsCodeIsRecognized(t *testing.T) {
	for _, code := range []string{
		"MISSING_REQUIRED_TOOLS",
		"MISSING_REQUIRED_TOOL",
		"missing-required-tools",
		"REQUIRED_TOOLS_MISSING",
		"REQUIRED_TOOL_MISSING",
		// Pre-existing forms, re-pinned so the added markers cannot regress them.
		"MISSING_TOOLS",
		"MISSING_CAPABILITY",
	} {
		if !isMissingCapabilityCode(code) {
			t.Errorf("isMissingCapabilityCode(%q) = false, want true", code)
		}
	}
	for _, code := range []string{
		"DEPENDENCY_NOT_MET",
		"DEPENDENCY_RESTORE_FAILED",
		"REQUIRED_REVIEW_MISSING",
		"MISSING_REQUIRED_INPUT",
		"TOOLCHAIN_VERSION_MISMATCH",
		"",
	} {
		if isMissingCapabilityCode(code) {
			t.Errorf("isMissingCapabilityCode(%q) = true, want false", code)
		}
	}
}

// TestMissingRequiredToolsBlockReclassifies proves the gap-closure end to end at
// the unit seam: the code now routes through the #2197 capability path, which is
// the right classification (a missing required tool is a system defect, not a
// per-item block) rather than the operational one.
func TestMissingRequiredToolsBlockReclassifies(t *testing.T) {
	result := apiv1.ResultEnvelope{
		Status:  apiv1.ResultBlocked,
		Summary: "the required tools were not present in this session",
		Error:   &apiv1.ErrorInfo{Code: "MISSING_REQUIRED_TOOLS", Message: "bash and edit are unavailable"},
	}
	reclassifyMissingCapabilityBlock(&result)
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	if result.Error.Code != ErrorCodeCapabilityUnsatisfied {
		t.Fatalf("error.Code = %q, want %q", result.Error.Code, ErrorCodeCapabilityUnsatisfied)
	}
	if result.Error.Retryable {
		t.Fatal("retryable = true, want false: the identical invocation reproduces the identical surface")
	}
	if !strings.Contains(result.Error.Message, "bash and edit are unavailable") {
		t.Fatalf("message = %q, want the agent's own cause preserved", result.Error.Message)
	}
	if got, ok := result.Outputs["capabilityUnsatisfied"].(bool); !ok || !got {
		t.Fatalf("outputs = %+v, want capabilityUnsatisfied=true", result.Outputs)
	}
}

// TestExecutorReclassifiesOperationalFailureBlock wires the conversion through
// Invoke — the seam every agentic stage's result passes through, and the reason
// the local runner and the Temporal engine cannot diverge on this: both consume
// the envelope this method returns.
func TestExecutorReclassifiesOperationalFailureBlock(t *testing.T) {
	adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
		return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
			Status:  apiv1.ResultBlocked,
			Summary: "npm ci failed, I cannot proceed",
			Error:   &apiv1.ErrorInfo{Code: "DEPENDENCY_RESTORE_FAILED", Message: "npm ci: ENOTEMPTY"},
		})
	}}
	exec := capabilityExecutor(t, adapter, []string{"shell"})

	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir()))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure so the declared gate is reachable", result.Status)
	}
	if result.Error == nil || result.Error.Code != "DEPENDENCY_RESTORE_FAILED" {
		t.Fatalf("error = %+v, want the producer's code preserved", result.Error)
	}
	if !result.Error.Retryable {
		t.Fatal("retryable = false, want true")
	}
	if !strings.Contains(result.Summary, "npm ci failed") {
		t.Fatalf("summary = %q, want the agent's narrative retained", result.Summary)
	}
}

// TestExecutorLeavesGenuineBlockedResult is the companion no-regression at the
// same seam: an ordinary dependency block still arrives as blocked, so the #544
// escalation path is entirely unchanged for it.
func TestExecutorLeavesGenuineBlockedResult(t *testing.T) {
	adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
		return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
			Status:  apiv1.ResultBlocked,
			Summary: "waiting on #441",
			Error:   &apiv1.ErrorInfo{Code: "DEPENDENCY_NOT_MET", Message: "issue 441 must merge first"},
		})
	}}
	exec := capabilityExecutor(t, adapter, []string{"shell"})

	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir()))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultBlocked {
		t.Fatalf("status = %q, want blocked: a genuine dependency block still escalates", result.Status)
	}
	if result.Error == nil || result.Error.Code != "DEPENDENCY_NOT_MET" {
		t.Fatalf("error = %+v, want DEPENDENCY_NOT_MET untouched", result.Error)
	}
}
