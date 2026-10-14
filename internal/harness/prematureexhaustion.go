package harness

import (
	"fmt"
	"slices"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/mcpio"
)

// ErrorCodePrematureExhaustionClaim is the failure code for an agent that
// reported its own execution window exhausted while the deadline the runtime
// actually enforced was still far away (#5871). The agent's claim is not an
// execution timeout: no deadline expired, and recording it as one sends an
// operator to tune a time limit that was never reached. The original code and
// message are kept in the rewritten error and in outputs.agentErrorCode.
const ErrorCodePrematureExhaustionClaim = "PREMATURE_EXHAUSTION_CLAIM"

// executionWindowClaimCodes are the agent-reported codes that assert the
// stage's own execution window ran out. Matched on the normalized code by
// exact equality, like operationalFailureCodeMarkers: an inner timeout the
// agent observed (a test suite, a provider call) is a real finding about the
// work and must never be rewritten by analogy, so this set grows only when a
// specific code is observed claiming the stage's own window.
var executionWindowClaimCodes = map[string]bool{
	// Observed verbatim on a production backlog-curation prioritize stage
	// that returned after 22s of a 30m window without inspecting its input.
	"EXECUTION_WINDOW_EXHAUSTED": true,
	// The runner's own wall-clock codes (telemetry.ErrCodeTimeout,
	// runner.RunDurationExceededErrorCode). Only the runtime observes those
	// deadlines; an agent authoring one is claiming a runner verdict, and
	// "timeout" would otherwise classify as the timeout error class.
	"TIMEOUT":               true,
	"RUN_DURATION_EXCEEDED": true,
}

// prematureClaimMaxElapsed is the share of the observed window that may have
// elapsed for an exhaustion claim to count as premature. Half is deliberately
// generous to the agent: a session that stops near its deadline because the
// remaining time could not cover the work keeps its own classification, and
// only a claim made "well before the real deadline" is rewritten.
const prematureClaimMaxElapsed = 0.5

func isExecutionWindowClaimCode(code string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	normalized = strings.NewReplacer("-", "_", " ", "_", ".", "_", ":", "_").Replace(normalized)
	return executionWindowClaimCodes[normalized]
}

// executionEvidence is what the executor itself observed about one adapter
// invocation, attached to the Outcome by the executor and never by an adapter.
type executionEvidence struct {
	window    *executiondeadline.Window
	returned  time.Time
	readiness *readinessCapture
}

// reclassifyPrematureExhaustionClaim is #5871. An agent that reports its
// execution window exhausted while the runtime's deadline is still far away
// has not timed out: it stopped early. The stage still fails — nothing it
// claimed is accepted, and no partial work is admitted — but it fails as
// PREMATURE_EXHAUSTION_CLAIM, carrying the authoritative deadline evidence
// and a concrete account of which inputs the agent inspected through which
// tools and what the goobers-io readiness check observed, so the operator
// sees an agent that gave up rather than a time limit to raise.
//
// The rewrite is retryable: a premature give-up is an agent-side contract
// miss like an omitted declared artifact, so the stage's own retry budget
// (retry.maxAttempts) covers it. A stage that declares no retries fails
// exactly as before, under the honest code.
//
// Runs on the shared harness result path, so the local runner and the
// Temporal engine observe the identical envelope.
func reclassifyPrematureExhaustionClaim(result *apiv1.ResultEnvelope, pointers []apiv1.ContextPointer, out Outcome) {
	if result == nil || result.Error == nil || !isExecutionWindowClaimCode(result.Error.Code) {
		return
	}
	if result.Status != apiv1.ResultFailure && result.Status != apiv1.ResultBlocked {
		return
	}
	started, deadline, ok := out.execution.window.Observed()
	if !ok {
		return // No bounded execution was observed: there is no deadline to judge by.
	}
	window := deadline.Sub(started)
	elapsed := out.execution.returned.Sub(started)
	if window <= 0 || elapsed < 0 || float64(elapsed) >= float64(window)*prematureClaimMaxElapsed {
		return
	}
	inspection := summarizeInputInspection(pointers, out.InputInspectionReceipts, out.InputInspectionReceiptsCollected)
	readiness := goobersIOReadinessDiagnostic(out.execution.readiness)
	agentCode := result.Error.Code
	cause := authoredCause(*result)

	if result.Outputs == nil {
		result.Outputs = map[string]interface{}{}
	}
	if result.Status == apiv1.ResultBlocked {
		result.Outputs["reclassifiedFromBlocked"] = true
	}
	result.Outputs["prematureExhaustionClaim"] = true
	result.Outputs["agentErrorCode"] = agentCode
	result.Outputs["executionDeadline"] = deadline.UTC().Format(time.RFC3339)
	result.Outputs["executionWindowSeconds"] = window.Seconds()
	result.Outputs["executionElapsedSeconds"] = elapsed.Seconds()
	result.Outputs["goobersIOInspectionCalls"] = float64(len(out.InputInspectionReceipts))
	result.Outputs["goobersIOReadiness"] = readiness.category
	if len(inspection.incomplete) > 0 {
		result.Outputs["inputsNotFullyInspected"] = strings.Join(inspection.incomplete, ",")
	}

	result.Status = apiv1.ResultFailure
	result.Error = &apiv1.ErrorInfo{
		Code:      ErrorCodePrematureExhaustionClaim,
		Retryable: true,
		Message: fmt.Sprintf(
			"the agent reported %s after %s of its %s execution window, but the runtime deadline %s had not been reached (%s remained), so this is not an execution timeout. %s %s Agent-reported cause: %s",
			agentCode, roundDuration(elapsed), roundDuration(window), deadline.UTC().Format(time.RFC3339),
			roundDuration(window-elapsed), inspection.text, readiness.text, cause),
	}
	result.Summary = "agent reported its execution window exhausted well before the runtime deadline: " + cause
}

func roundDuration(d time.Duration) time.Duration {
	if d >= time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(100 * time.Millisecond)
}

type readinessDiagnostic struct {
	category string
	text     string
}

func goobersIOReadinessDiagnostic(capture *readinessCapture) readinessDiagnostic {
	report, ok := capture.goobersIO()
	if !ok {
		return readinessDiagnostic{
			category: "unreported",
			text:     "goobers-io readiness: no readiness report for this session (the server was not registered or the session never reached its readiness check).",
		}
	}
	text := fmt.Sprintf("goobers-io readiness: %s (source %s; connection %s; inventory %s).",
		report.Category, report.Source, report.Connection, report.Inventory)
	if report.Category == "check_unobservable" {
		text += " Readiness was not observable on this adapter, so tool availability is neither confirmed nor refuted by the check."
	}
	return readinessDiagnostic{category: report.Category, text: text}
}

type inputInspectionSummary struct {
	// incomplete names every context input the agent did not read in full.
	incomplete []string
	text       string
}

// maxDiagnosedInputs bounds the per-input detail in the message; the full
// list of incompletely inspected inputs is always in the outputs.
const maxDiagnosedInputs = 10

// summarizeInputInspection reports, per artifact-backed context input, what
// the agent's goobers-io receipts show it read before returning. Receipts are
// tool-owned records written by the goobers-io server itself, never the
// agent's own account of what it read.
func summarizeInputInspection(pointers []apiv1.ContextPointer, receipts []mcpio.InputInspectionReceipt, collected bool) inputInspectionSummary {
	var inputs []apiv1.ContextPointer
	for _, pointer := range pointers {
		if pointer.Artifact != nil {
			inputs = append(inputs, pointer)
		}
	}
	if len(inputs) == 0 {
		return inputInspectionSummary{text: "The stage was handed no artifact-backed context inputs."}
	}
	if !collected {
		var summary inputInspectionSummary
		for _, input := range inputs {
			summary.incomplete = append(summary.incomplete, input.Name)
		}
		summary.text = fmt.Sprintf("Input inspection is unobservable: goobers-io receipts were not collected for this session, so nothing shows the agent read any of its %d context input(s).", len(inputs))
		return summary
	}
	listed, failed := false, 0
	for _, receipt := range receipts {
		if !receipt.Success {
			failed++
			continue
		}
		if receipt.Tool == "list_inputs" {
			listed = true
		}
	}
	var summary inputInspectionSummary
	var details []string
	for i, input := range inputs {
		state, complete := inputInspectionState(input.Name, receipts)
		if !complete {
			summary.incomplete = append(summary.incomplete, input.Name)
		}
		if i < maxDiagnosedInputs {
			details = append(details, fmt.Sprintf("%s (%d bytes): %s", input.Name, input.Artifact.Size, state))
		}
	}
	if len(inputs) > maxDiagnosedInputs {
		details = append(details, fmt.Sprintf("and %d more", len(inputs)-maxDiagnosedInputs))
	}
	listState := "not called"
	if listed {
		listState = "called"
	}
	summary.text = fmt.Sprintf("Input inspection before the claim: %d goobers-io inspection call(s) (%d failed); list_inputs %s; %s.",
		len(receipts), failed, listState, strings.Join(details, "; "))
	return summary
}

// inputInspectionState describes one input's coverage by successful
// read_input and grep_input receipts. complete is true only when the read
// ranges cover every line, the evidence a whole-batch consumer needs before
// it may act on the batch.
func inputInspectionState(name string, receipts []mcpio.InputInspectionReceipt) (state string, complete bool) {
	var ranges [][2]int
	total, grepped := 0, false
	for _, receipt := range receipts {
		if !receipt.Success || receipt.Input != name {
			continue
		}
		switch receipt.Tool {
		case "read_input":
			if receipt.TotalLines == 0 {
				return "fully read (empty input)", true
			}
			total = receipt.TotalLines
			if receipt.StartLine >= 1 && receipt.EndLine >= receipt.StartLine {
				ranges = append(ranges, [2]int{receipt.StartLine, min(receipt.EndLine, receipt.TotalLines)})
			}
		case "grep_input":
			grepped = true
		}
	}
	covered := coveredLines(ranges)
	switch {
	case total > 0 && covered >= total:
		return "fully read", true
	case covered > 0:
		return fmt.Sprintf("partially read (%d of %d lines)", covered, total), false
	case grepped:
		return "searched with grep_input but never read", false
	default:
		return "not inspected", false
	}
}

// coveredLines counts the distinct lines a set of inclusive ranges covers.
func coveredLines(ranges [][2]int) int {
	slices.SortFunc(ranges, func(a, b [2]int) int { return a[0] - b[0] })
	covered, through := 0, 0
	for _, r := range ranges {
		start := max(r[0], through+1)
		if r[1] >= start {
			covered += r[1] - start + 1
			through = r[1]
		}
	}
	return covered
}
