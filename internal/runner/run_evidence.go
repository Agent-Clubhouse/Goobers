package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
)

type transcriptToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type transcriptEvent struct {
	Role     string              `json:"role"`
	ToolCall *transcriptToolCall `json:"tool_call,omitempty"`
}

type remediationEvidenceInspectionError struct {
	info *apiv1.ErrorInfo
	// digest is the rejected attempt's diff digest — the identity the
	// rejection budget is pinned to (#3375), so a later attempt that actually
	// changes the branch starts counting again from zero.
	digest string
}

func (e *remediationEvidenceInspectionError) Error() string {
	if e == nil || e.info == nil {
		return "remediation failure evidence was not inspected"
	}
	return e.info.Message
}

func normalizeInputToolName(name string) string {
	return strings.TrimPrefix(name, "functions.")
}

func parseTranscriptInputInspection(transcript []byte, required map[string]struct{}) (bool, map[string]bool) {
	inspected := make(map[string]bool, len(required))
	var sawListInputs bool
	for _, line := range bytes.Split(transcript, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event transcriptEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if event.Role != "assistant" || event.ToolCall == nil {
			continue
		}
		toolName := normalizeInputToolName(event.ToolCall.Name)
		switch toolName {
		case "goobers-io-list_inputs":
			sawListInputs = true
		case "goobers-io-read_input", "goobers-io-grep_input":
			var args struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(event.ToolCall.Arguments, &args); err != nil {
				continue
			}
			if _, ok := required[args.Name]; ok {
				inspected[args.Name] = true
			}
		}
	}
	return sawListInputs, inspected
}

func pointerValidationErrorMessage(missing []string, sawListInputs bool) string {
	if len(missing) == 0 && sawListInputs {
		return ""
	}
	listStatus := "not called"
	if sawListInputs {
		listStatus = "called"
	}
	return fmt.Sprintf(
		"required context pointers were not inspected before DEPENDENCY_NOT_MET (list_inputs: %s; unread pointers: %s)",
		listStatus,
		strings.Join(missing, ", "),
	)
}

func dependencyValidationMessage(validation string, result apiv1.ResultEnvelope) string {
	if result.Error == nil || strings.TrimSpace(result.Error.Message) == "" {
		return validation
	}
	return validation + "; original dependency error: " + result.Error.Message
}

func dependencyContextInspectionError(cause, validation string, result apiv1.ResultEnvelope) string {
	message := cause
	if validation != "" {
		message += "; " + validation
	}
	return dependencyValidationMessage(message, result)
}

// validateDependencyNotMet checks whether a DEPENDENCY_NOT_MET failure in a
// repass scenario has evidence of input inspection. When a stage reports
// blocked with DEPENDENCY_NOT_MET but did not attempt to inspect provided
// context pointers using list_inputs plus read_input/grep_input for each named
// pointer, this returns a validation error with code CONTEXT_NOT_INSPECTED.
func (r *Runner) validateDependencyNotMet(jr executionJournal, _ string, result apiv1.ResultEnvelope, requiredPointers []apiv1.ContextPointer) *apiv1.ErrorInfo {
	if result.Transcript == nil {
		return &apiv1.ErrorInfo{
			Code: ContextNotInspectedCode,
			Message: dependencyContextInspectionError(
				"cannot inspect required context: result transcript pointer is missing",
				pointerValidationErrorMessage(requiredContextPointerNames(requiredPointers), false),
				result,
			),
		}
	}
	requiredNames := requiredContextPointerNames(requiredPointers)

	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return &apiv1.ErrorInfo{
			Code: ContextNotInspectedCode,
			Message: dependencyContextInspectionError(
				fmt.Sprintf("cannot inspect required context: open run journal %q: %v", jr.Dir(), err),
				pointerValidationErrorMessage(requiredNames, false),
				result,
			),
		}
	}
	transcriptRef := journal.Ref{
		Path:      result.Transcript.Path,
		Digest:    result.Transcript.Digest,
		Size:      result.Transcript.Size,
		Integrity: result.Transcript.Integrity,
	}
	transcript, err := rd.SpanBytes(transcriptRef)
	if err != nil {
		return &apiv1.ErrorInfo{
			Code: ContextNotInspectedCode,
			Message: dependencyContextInspectionError(
				fmt.Sprintf("cannot inspect required context: read transcript %q: %v", transcriptRef.Path, err),
				pointerValidationErrorMessage(requiredNames, false),
				result,
			),
		}
	}
	// The verdict itself is the PURE function both runners share
	// (parity3882.go): this method's job is resolving the transcript bytes,
	// which is the only part that differs between a run directory and a
	// worker's span store.
	return ValidateDependencyNotMetTranscript(transcript, requiredPointers, result)
}

func (r *Runner) validateDependencyResult(jr executionJournal, stage string, result apiv1.ResultEnvelope, invocationPointers []apiv1.ContextPointer) apiv1.ResultEnvelope {
	if !AppliesDependencyValidation(result, invocationPointers) {
		return result
	}
	return RejectDependencyResult(result, r.validateDependencyNotMet(jr, stage, result, invocationPointers))
}

// ReceiptLineRange is one inclusive line span of an evidence artifact an
// attempt is obliged to have read (or claims to have read). Exported with the
// obligation it belongs to; receiptLineRange remains the in-package name.
type ReceiptLineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type receiptLineRange = ReceiptLineRange

func parseReceiptInputInspection(jr executionJournal, stage string, required map[string]string, actionable map[string]actionableEvidence) (bool, map[string]bool, error) {
	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return false, nil, err
	}
	events, err := rd.Events()
	if err != nil {
		return false, nil, err
	}
	start := -1
	for i, event := range events {
		if event.Type == journal.EventStageStarted && event.Stage == stage {
			start = i
		}
	}
	if start < 0 {
		return false, nil, fmt.Errorf("stage %q has no invocation boundary", stage)
	}
	inspected := make(map[string]bool, len(required))
	readRanges := make(map[string][]receiptLineRange, len(required))
	readTotals := make(map[string]int, len(required))
	invalidReadTotals := make(map[string]bool, len(required))
	var collected, sawListInputs bool
	for _, event := range events[start+1:] {
		// Harness annotations use the invocation-qualified task ID rather than
		// the bare workflow stage. The latest stage.started boundary above is
		// the authoritative invocation scope.
		if event.Type != journal.EventRunnerAnnotation ||
			event.Stage != stage && !strings.HasSuffix(event.Stage, ":"+stage) ||
			event.Runner["kind"] != "goobers-io-input-inspection-receipts" {
			continue
		}
		collected = true
		data, err := json.Marshal(event.Runner["receipts"])
		if err != nil {
			return true, nil, fmt.Errorf("encode receipt annotation: %w", err)
		}
		var receipts []mcpio.InputInspectionReceipt
		if err := json.Unmarshal(data, &receipts); err != nil {
			return true, nil, fmt.Errorf("decode receipt annotation: %w", err)
		}
		for _, receipt := range receipts {
			if !receipt.Success {
				continue
			}
			switch receipt.Tool {
			case "list_inputs":
				sawListInputs = true
			case "read_input":
				expectedDigest, ok := required[receipt.Input]
				if !ok || expectedDigest != "" && receipt.InputDigest != expectedDigest {
					continue
				}
				if receipt.TotalLines == 0 {
					inspected[receipt.Input] = true
					continue
				}
				if receipt.StartLine < 1 || receipt.EndLine < receipt.StartLine || receipt.EndLine > receipt.TotalLines {
					continue
				}
				if total, exists := readTotals[receipt.Input]; exists && total != receipt.TotalLines {
					invalidReadTotals[receipt.Input] = true
					continue
				}
				readTotals[receipt.Input] = receipt.TotalLines
				readRanges[receipt.Input] = append(readRanges[receipt.Input], receiptLineRange{
					Start: receipt.StartLine,
					End:   receipt.EndLine,
				})
			case "grep_input":
				expectedDigest, ok := required[receipt.Input]
				if ok && (expectedDigest == "" || receipt.InputDigest == expectedDigest) && len(receipt.MatchLines) > 0 &&
					receiptMatchesActionableEvidence(receipt.Pattern, actionable[receipt.Input]) {
					inspected[receipt.Input] = true
				}
			}
		}
	}
	for input, ranges := range readRanges {
		if inspected[input] || invalidReadTotals[input] {
			continue
		}
		slices.SortFunc(ranges, func(a, b receiptLineRange) int {
			return a.Start - b.Start
		})
		coveredThrough := 0
		for _, lineRange := range ranges {
			if lineRange.Start > coveredThrough+1 {
				break
			}
			if lineRange.End > coveredThrough {
				coveredThrough = lineRange.End
			}
		}
		if coveredThrough >= readTotals[input] {
			inspected[input] = true
			continue
		}
		if requirement, ok := actionable[input]; ok && receiptRangesOverlap(readRanges[input], requirement.Ranges) {
			inspected[input] = true
		}
	}
	return collected && sawListInputs, inspected, nil
}

func receiptRangesOverlap(received, required []receiptLineRange) bool {
	for _, got := range received {
		for _, want := range required {
			if got.Start <= want.End && want.Start <= got.End {
				return true
			}
		}
	}
	return false
}

func receiptMatchesActionableEvidence(pattern string, requirement actionableEvidence) bool {
	if len(requirement.Signatures) == 0 {
		return true
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	for _, signature := range requirement.Signatures {
		if re.MatchString(signature) {
			return true
		}
	}
	return false
}

func (r *Runner) validateRemediationEvidence(jr executionJournal, stage string, result apiv1.ResultEnvelope, requiredPointers []apiv1.ContextPointer) *apiv1.ErrorInfo {
	requiredNames := requiredContextPointerNames(requiredPointers)
	requiredSet := make(map[string]string, len(requiredPointers))
	for _, pointer := range requiredPointers {
		if pointer.Artifact != nil {
			requiredSet[pointer.Name] = pointer.Artifact.Digest
		} else {
			requiredSet[pointer.Name] = ""
		}
	}
	actionable := remediationEvidenceRequirementsFromJournal(jr, stage)
	sawListInputs, inspected, err := parseReceiptInputInspection(jr, stage, requiredSet, actionable)
	if err != nil {
		return &apiv1.ErrorInfo{
			Code: gate.ReasonRemediationEvidenceNotInspected,
			Message: dependencyValidationMessage(
				fmt.Sprintf("cannot inspect trusted goobers-io receipts: %v", err),
				result,
			),
		}
	}
	missing := make([]string, 0, len(requiredNames))
	for _, name := range requiredNames {
		if !inspected[name] {
			missing = append(missing, name)
		}
	}
	if sawListInputs && len(missing) == 0 {
		if classificationErr := validateUnchangedRemediationClassification(result); classificationErr != "" {
			return &apiv1.ErrorInfo{
				Code:    gate.ReasonRemediationEvidenceNotInspected,
				Message: dependencyValidationMessage(classificationErr, result),
			}
		}
		return nil
	}
	return &apiv1.ErrorInfo{
		Code: gate.ReasonRemediationEvidenceNotInspected,
		Message: dependencyValidationMessage(
			strings.Replace(
				pointerValidationErrorMessage(missing, sawListInputs),
				"before DEPENDENCY_NOT_MET",
				"before accepting unchanged remediation",
				1,
			),
			result,
		),
	}
}

func remediationEvidenceRequirementsFromJournal(jr executionJournal, stage string) map[string]actionableEvidence {
	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return nil
	}
	events, err := rd.Events()
	if err != nil {
		return nil
	}
	requirements := make(map[string]actionableEvidence)
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != RemediationEvidenceRequiredKind ||
			event.Stage != stage {
			continue
		}
		data, err := json.Marshal(event.Runner["actionableEvidence"])
		if err != nil {
			continue
		}
		var entries []actionableEvidence
		if json.Unmarshal(data, &entries) == nil {
			for _, entry := range entries {
				requirements[entry.Pointer] = entry
			}
		}
	}
	return requirements
}

const remediationClassificationOutput = "remediationClassification"

func validateUnchangedRemediationClassification(result apiv1.ResultEnvelope) string {
	raw, ok := result.Outputs[remediationClassificationOutput]
	classification, valid := raw.(string)
	if !ok || !valid {
		return fmt.Sprintf("unchanged remediation must set outputs.%s to environmental, flaky, obsolete, or non-actionable", remediationClassificationOutput)
	}
	switch strings.ToLower(strings.TrimSpace(classification)) {
	case "environmental", "flaky", "obsolete", "non-actionable":
		// accepted classification
	default:
		return fmt.Sprintf("unchanged remediation must set outputs.%s to environmental, flaky, obsolete, or non-actionable", remediationClassificationOutput)
	}
	if strings.TrimSpace(result.Summary) == "" {
		return "unchanged remediation must explain why the inspected failure is non-actionable in summary"
	}
	return ""
}

func requiredContextPointerNames(pointers []apiv1.ContextPointer) []string {
	if len(pointers) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(pointers))
	names := make([]string, 0, len(pointers))
	for _, pointer := range pointers {
		name := strings.TrimSpace(pointer.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
