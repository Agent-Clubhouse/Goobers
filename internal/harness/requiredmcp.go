package harness

import (
	"errors"
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

// ErrorCodeRequiredMCPEnterpriseBlocked is the failure code for a stage whose
// required goobers-io server the CLI refused under an enterprise customization
// lockdown that permits only plugin or managed MCP servers (#6358).
//
// Running goobers-io under that lockdown is unsupported. The code is distinct
// from the third-party policy code because there is no organization setting
// to enable here, and distinct from the availability code because retrying
// cannot help: the lockdown refuses the server on every attempt.
const ErrorCodeRequiredMCPEnterpriseBlocked = "HARNESS_REQUIRED_MCP_ENTERPRISE_BLOCKED"

var errRequiredMCPUnavailable = errors.New("required MCP server unavailable")

// errRequiredMCPEnterpriseBlocked marks a required-MCP run error whose cause
// the CLI's own log names as an enterprise customization lockdown (#6358).
var errRequiredMCPEnterpriseBlocked = errors.New("required MCP server blocked by enterprise policy")

// requiredMCPEnterpriseBlockedDetail is the stage error text for #6358. It
// says what happened, that the configuration is unsupported, and where to go.
var requiredMCPEnterpriseBlockedDetail = fmt.Sprintf(
	"this stage requires the %s MCP server for artifact and context I/O, and "+
		"the Copilot CLI refused it under an enterprise customization lockdown that permits only plugin or "+
		"managed MCP servers. Running %s under that lockdown is unsupported; run this stage on an adapter "+
		"or account where the lockdown does not apply (see docs/guides/goobers-io-mcp.md).",
	goobersIOServerName, goobersIOServerName)

// classifyCopilotEnterpriseBlock names an enterprise customization lockdown as
// the cause of a required-MCP run error when the CLI's own log for this
// invocation shows goobers-io refused that way (#6358). Any other error, or a
// log that does not show the refusal, is returned unchanged.
func classifyCopilotEnterpriseBlock(runErr error, logDir string) error {
	if !errors.Is(runErr, errRequiredMCPRejected) && !errors.Is(runErr, errRequiredMCPUnavailable) {
		return runErr
	}
	if !hasKey(copilotMCPEnterpriseBlockedServers(logDir), goobersIOServerName) {
		return runErr
	}
	return fmt.Errorf("%w: %s: %w", errRequiredMCPEnterpriseBlocked, requiredMCPEnterpriseBlockedDetail, runErr)
}

func requiredMCPInfrastructureFailure(failures []MCPServerFailure) error {
	for _, failure := range failures {
		if failure.Server == goobersIOServerName && failure.Status == copilotMCPStatusRemovedAfterConnect {
			return fmt.Errorf("%w: %s status %q", errRequiredMCPUnavailable, failure.Server, failure.Status)
		}
	}
	return nil
}

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

	if status == copilotMCPStatusEnterpriseBlocked {
		result.Error = &apiv1.ErrorInfo{
			Code: ErrorCodeRequiredMCPEnterpriseBlocked,
			// Not retryable: the lockdown refuses the server on every attempt.
			Retryable: false,
			Message:   "blocked by enterprise policy: " + requiredMCPEnterpriseBlockedDetail,
		}
		result.Summary = "required " + goobersIOServerName + " MCP server blocked by enterprise policy"
		return
	}
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
