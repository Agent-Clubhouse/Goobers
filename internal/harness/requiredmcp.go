package harness

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ErrorCodeRequiredMCPRejected is the failure code for a stage whose required
// goobers-io server the CLI refused under its third-party MCP policy (#2955).
//
// Its own code because the operator action is specific and administrative:
// enable the third-party MCP policy for this organization, or run the stage on
// an adapter that does not need the server. A generic failure sent a human to
// read the diff instead.
const ErrorCodeRequiredMCPRejected = "HARNESS_REQUIRED_MCP_REJECTED"

// ErrorCodeRequiredMCPUnavailable is the failure code for a required
// goobers-io server that policy allowed but that never became usable (#2955).
//
// Deliberately distinct from the policy code: this is a fault to investigate,
// that one is a setting to change, and telling an operator the wrong one costs
// the whole diagnosis.
const ErrorCodeRequiredMCPUnavailable = "HARNESS_REQUIRED_MCP_UNAVAILABLE"

// refuseWhenRequiredMCPUnavailable fails a stage whose required artifact and
// context tools were never reachable, whatever the agent reported (#2955).
//
// Goobers wires the credential-free, workspace-scoped goobers-io server into
// eligible stages and then instructs the model to use its artifact and input
// tools instead of ordinary file tools. When the CLI's third-party MCP policy
// is disabled it skips the server and the invocation carries on: the stage's
// contract can no longer be satisfied, but nothing said so, and the run spends
// an agentic attempt and workflow budget producing an answer nothing should
// trust.
//
// #3356 already detects the unavailable server and journals it, deliberately
// as an annotation only — "the run's own outcome is untouched". That was the
// right conservative first step for ANY registered server, since a goober may
// declare an optional one. It is not sufficient for goobers-io specifically,
// which is not optional: the harness registers it, the prompt depends on it,
// and no stage that needs it can do its work without it.
//
// So the override is scoped to that one server, and the agent's own status is
// not consulted. A model that reports success without the tools its
// instructions told it to use has not done the declared work, and its report
// is the least reliable evidence available about that.
func refuseWhenRequiredMCPUnavailable(result *apiv1.ResultEnvelope, failures []MCPServerFailure) {
	if result == nil {
		return
	}
	status, found := "", false
	for _, failure := range failures {
		if failure.Server == goobersIOServerName {
			status, found = failure.Status, true
			break
		}
	}
	if !found {
		return
	}

	if result.Outputs == nil {
		result.Outputs = map[string]interface{}{}
	}
	result.Outputs["requiredMCPUnavailable"] = goobersIOServerName
	result.Outputs["requiredMCPStatus"] = status
	result.Status = apiv1.ResultFailure

	if status == copilotMCPStatusPolicyRejected {
		result.Error = &apiv1.ErrorInfo{
			Code: ErrorCodeRequiredMCPRejected,
			// Not retryable: the policy will refuse the server again on every
			// attempt, so a repass spends budget to reach the same place.
			Retryable: false,
			Message: fmt.Sprintf(
				"this stage requires the %s MCP server for artifact and context I/O, and the CLI refused it "+
					"under the third-party MCP policy, so the stage contract could not be satisfied. Enable the "+
					"third-party MCP policy for this organization, or run this stage on the claude-code adapter, "+
					"which registers %s without that policy.",
				goobersIOServerName, goobersIOServerName),
		}
		result.Summary = "required " + goobersIOServerName + " MCP server rejected by third-party MCP policy"
		return
	}

	result.Error = &apiv1.ErrorInfo{
		Code: ErrorCodeRequiredMCPUnavailable,
		// Retryable, unlike the policy case: a server that failed to launch or
		// never completed its handshake may well come up on the next attempt.
		Retryable: true,
		Message: fmt.Sprintf(
			"this stage requires the %s MCP server for artifact and context I/O, and it was registered but "+
				"never usable (status %q), so the stage contract could not be satisfied. This is a server or "+
				"environment fault rather than a policy decision.",
			goobersIOServerName, status),
	}
	result.Summary = "required " + goobersIOServerName + " MCP server was not available"
}
