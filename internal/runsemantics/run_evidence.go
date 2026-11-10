package runsemantics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type transcriptToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type transcriptEvent struct {
	Role     string              `json:"role"`
	ToolCall *transcriptToolCall `json:"tool_call,omitempty"`
}

func normalizeInputToolName(name string) string {
	return strings.TrimPrefix(name, "functions.")
}

// ParseTranscriptInputInspection records which required inputs a transcript inspected.
func ParseTranscriptInputInspection(transcript []byte, required map[string]struct{}) (bool, map[string]bool) {
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

// PointerValidationErrorMessage describes missing input inspection evidence.
func PointerValidationErrorMessage(missing []string, sawListInputs bool) string {
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

// DependencyValidationMessage combines a validation refusal with the stage result.
func DependencyValidationMessage(validation string, result apiv1.ResultEnvelope) string {
	if result.Error == nil || strings.TrimSpace(result.Error.Message) == "" {
		return validation
	}
	return validation + "; original dependency error: " + result.Error.Message
}

// ReceiptLineRange is one inclusive line span of an evidence artifact an
// attempt is obliged to have read (or claims to have read). Exported with the
// obligation it belongs to; receiptLineRange remains the in-package name.
type ReceiptLineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type receiptLineRange = ReceiptLineRange

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
