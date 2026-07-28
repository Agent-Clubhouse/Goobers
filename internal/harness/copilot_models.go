package harness

import (
	"context"
	"fmt"

	copilot "github.com/github/copilot-sdk/go"
)

type sdkCopilotModelLister struct{}

func (sdkCopilotModelLister) ListModels(ctx context.Context, command, env []string) ([]CopilotModelInfo, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("harness: copilot-cli: model discovery command is empty")
	}
	client := copilot.NewClient(&copilot.ClientOptions{
		Connection: copilot.StdioConnection{
			Path: command[0],
			Args: command[1:],
			Env:  env,
		},
		LogLevel:        "error",
		UseLoggedInUser: copilot.Bool(true),
	})
	defer client.ForceStop()
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("start Copilot model discovery: %w", err)
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Copilot models: %w", err)
	}
	result := make([]CopilotModelInfo, 0, len(models))
	for _, model := range models {
		result = append(result, mapCopilotModelInfo(model))
	}
	return result, nil
}

func mapCopilotModelInfo(model copilot.ModelInfo) CopilotModelInfo {
	policyState := ""
	if model.Policy != nil {
		policyState = model.Policy.State
	}
	return CopilotModelInfo{
		ID:                        model.ID,
		PolicyState:               policyState,
		SupportedReasoningEfforts: append([]string(nil), model.SupportedReasoningEfforts...),
	}
}
