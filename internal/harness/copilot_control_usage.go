package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Finish after the last completion/recovery turn and the actual-session MCP
// check, before reading native captures. Persistent headless sessions otherwise
// have not emitted their shutdown totals when the ordinary CLI reader runs.
func finalizeControlledCopilot(ctx context.Context, runner ProcessRunner) error {
	controlled, ok := runner.(*copilotControlledRunner)
	if !ok || !controlled.ready {
		return nil
	}
	session, ok := controlled.session.(sdkRequiredMCPSession)
	if !ok {
		return nil
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	usage, usageErr := session.session.RPC.Usage.GetMetrics(finish)
	_, shutdownErr := session.session.RPC.Shutdown(finish, nil)
	if usageErr == nil && usage != nil {
		data, err := json.Marshal(usage)
		if err != nil {
			usageErr = err
		} else {
			metrics, models, valid := copilotUsageMetrics(data, true)
			if !valid {
				metrics, models, valid = copilotUsageMetrics(data, false)
			}
			if valid {
				controlled.usage = transcriptCapture{metrics: metrics, modelUsage: models}
			} else {
				usageErr = fmt.Errorf("invalid session usage response")
			}
		}
	}
	// Preserve categorical capture errors without echoing native server details.
	var result error
	if usageErr != nil {
		result = fmt.Errorf("Copilot session usage capture failed")
	}
	if shutdownErr != nil {
		result = errors.Join(result, fmt.Errorf("Copilot session finalization failed"))
	}
	return result
}

func applyControlledCopilotUsage(out *Outcome, runner ProcessRunner) {
	if controlled, ok := runner.(*copilotControlledRunner); ok && controlled.usage.metrics != nil {
		out.Metrics, out.ModelUsage = controlled.usage.metrics, controlled.usage.modelUsage
	}
}
