package handoffcheck

import (
	"context"
	"encoding/json"
)

// OutputKey is the runner-authored ResultEnvelope output carrying handoff
// validation ground truth for the stage's consumed inputs.
const OutputKey = "handoffValidation"

// InputValid summarizes the deterministic ground truth for the stage input.
type InputValid string

// Aggregate handoff-validation states.
const (
	InputValidUnknown InputValid = "unknown"
	InputValidTrue    InputValid = "true"
	InputValidFalse   InputValid = "false"
)

// ReportEntry is one validated schema-bound handoff consumed by a stage.
type ReportEntry struct {
	Input    string  `json:"input,omitempty"`
	Producer string  `json:"producer,omitempty"`
	Slot     string  `json:"slot,omitempty"`
	SchemaID string  `json:"schemaId,omitempty"`
	Valid    bool    `json:"valid"`
	Issues   []Issue `json:"issues,omitempty"`
}

// Report is the runner-authored deterministic handoff-validation record for one
// stage invocation.
type Report struct {
	InputValid InputValid    `json:"inputValid"`
	Entries    []ReportEntry `json:"entries,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// Bool reports the aggregate input-validity decision when it is known.
func (r Report) Bool() (valid, known bool) {
	switch r.InputValid {
	case InputValidTrue:
		return true, true
	case InputValidFalse:
		return false, true
	default:
		return false, false
	}
}

type reportContextKey struct{}

// WithReport stores report on ctx for runner-owned plumbing that must not be
// exposed to the agent prompt.
func WithReport(ctx context.Context, report Report) context.Context {
	return context.WithValue(ctx, reportContextKey{}, report)
}

// ReportFromContext retrieves a runner-authored handoff-validation report from
// ctx.
func ReportFromContext(ctx context.Context) (Report, bool) {
	if ctx == nil {
		return Report{}, false
	}
	report, ok := ctx.Value(reportContextKey{}).(Report)
	return report, ok
}

// ReportFromOutputs extracts a handoff-validation report from ResultEnvelope
// outputs. It accepts both the in-memory Report value and its generic
// map[string]any journal-decoded form.
func ReportFromOutputs(outputs map[string]interface{}) (Report, bool) {
	if len(outputs) == 0 {
		return Report{}, false
	}
	raw, ok := outputs[OutputKey]
	if !ok || raw == nil {
		return Report{}, false
	}
	switch value := raw.(type) {
	case Report:
		return value, true
	case *Report:
		if value == nil {
			return Report{}, false
		}
		return *value, true
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return Report{}, false
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, false
	}
	return report, true
}
