package harness

import (
	"encoding/json"
	"errors"
	"strings"
)

// Unlike a malformed completion, this is a valid failure result. The adapter
// gives it one continuation, then the executor retains the final failure.
var errIncompleteWork = errors.New("harness: agent explicitly reported INCOMPLETE_WORK")

func incompleteWorkCompletion(req RunRequest, payload []byte) error {
	if req.Mode != ModeInvoke {
		return nil
	}
	var result struct {
		Status string `json:"status"`
		Error  *struct {
			Code      string            `json:"code"`
			Retryable *bool             `json:"retryable"`
			Causes    []json.RawMessage `json:"causes"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &result) != nil || result.Status != "failure" ||
		result.Error == nil || result.Error.Code != "INCOMPLETE_WORK" ||
		len(result.Error.Causes) != 0 || result.Error.Retryable != nil && !*result.Error.Retryable {
		return nil
	}
	return errIncompleteWork
}

func incompleteWorkRepairProblem() string {
	return "Your previous turn explicitly reported INCOMPLETE_WORK, not a completed task. " +
		"Continue the remaining task using tools in the existing workspace, then report the actual outcome. " +
		"Setup and context loading alone are not completion. Do not invent a response-boundary deadline. " +
		"This is the single continuation within the original execution budget, not a new workflow attempt. " +
		"If a concrete operation fails, access is denied, or operator intervention is required, " +
		"report that specific failure instead of claiming success."
}

func copilotIncompleteWorkCommand(runner ProcessRunner, argv []string) ([]string, error) {
	command := append([]string(nil), argv...)
	if _, controlled := runner.(*copilotControlledRunner); controlled {
		// Its existing live RPC session receives the next prompt directly.
		return command, nil
	}
	for i, arg := range command {
		if arg == "--session-id" && i+1 < len(command) && command[i+1] != "" {
			command[i] = "--resume"
			return command, nil
		}
		if arg == "--resume" && i+1 < len(command) && command[i+1] != "" ||
			strings.HasPrefix(arg, "--resume=") && len(arg) > len("--resume=") {
			return command, nil
		}
	}
	return nil, errors.New("harness: cannot continue INCOMPLETE_WORK without a resumable Copilot session")
}
