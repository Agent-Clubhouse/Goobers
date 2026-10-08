package runner

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
)

// MissingExpectedOutputsCode is the stage error code for a successful shell
// stage that did not emit a key it declared in expectedOutputs (#5175).
const MissingExpectedOutputsCode = "missing_expected_outputs"

// EnforceExpectedOutputs fails a successful result that lacks any of task's
// declared expectedOutputs when v30.EnforcesExpectedOutputs applies. The failure
// names the declared result file and the missing keys, never file contents.
// Non-success results are returned unchanged: they already fail or report no
// work, and their own diagnostics stay authoritative.
func EnforceExpectedOutputs(dslVersion string, task apiv1.Task, result apiv1.ResultEnvelope) apiv1.ResultEnvelope {
	if result.Status != apiv1.ResultSuccess || !v30.EnforcesExpectedOutputs(dslVersion, task) {
		return result
	}
	var missing []string
	for _, key := range task.ExpectedOutputs {
		if _, ok := result.Outputs[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return result
	}
	message := missingExpectedOutputsMessage(task, missing)
	result.Status = apiv1.ResultFailure
	result.Error = &apiv1.ErrorInfo{Code: MissingExpectedOutputsCode, Message: message, Retryable: false}
	result.Summary = "declared expectedOutputs missing"
	return result
}

func missingExpectedOutputsMessage(task apiv1.Task, missing []string) string {
	resultFile := strings.TrimSpace(task.Inputs["resultFile"])
	if resultFile == "" {
		return fmt.Sprintf(
			"stage %q declares expectedOutputs %q but did not emit %q: it declares no inputs.resultFile, the only channel a shell stage emits outputs through",
			task.Name, task.ExpectedOutputs, missing)
	}
	return fmt.Sprintf(
		"stage %q declares expectedOutputs %q but its result file %q did not provide %q; the file must be a flat JSON object (UTF-8, optional byte-order mark) with a string, number, or boolean value for each declared key",
		task.Name, task.ExpectedOutputs, resultFile, missing)
}

// dslVersion is the pinned DSL version of the run's workflow, or "" when the
// input carries no compiled machine.
func (in StartInput) dslVersion() string {
	if in.Machine == nil {
		return ""
	}
	return in.Machine.Def.DSLVersion
}
