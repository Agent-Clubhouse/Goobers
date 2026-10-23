package main

import (
	"os/exec"

	"github.com/goobers/goobers/providers"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/worktree"
)

type runnerCompositionInput = runtimewiring.ExecutorCompositionInput

var runnerLookPath = exec.LookPath

func buildRunnerConfig(input runnerCompositionInput) (runtimewiring.ExecutorConfig, *worktree.Manager, error) {
	binary, err := workerExecutorBinary()
	if err != nil {
		return runtimewiring.ExecutorConfig{}, nil, err
	}
	input.ExecutorBinary = binary
	input.Diagnostics, input.DiagnosticsMaxBytes = diagnosticsMode, diagnosticsMaxOutputBytes
	input.LookPath = runnerLookPath
	input.ConfigureWorktree = func(manager *worktree.Manager) error {
		cloneURL := runtimewiring.RepoCloneURL
		if cloneURL == nil {
			cloneURL = providers.CloneURL
		}
		option, err := runtimewiring.RecoveryCleanupOption(input.Layout, input.Config, manager.Root, cloneURL, input.SharedRegistry, input.Telemetry)
		if err != nil {
			return err
		}
		option(manager)
		return nil
	}
	input.BuildDeterministic = func(stage runtimewiring.DeterministicExecutorInput) (invoke.Deterministic, error) {
		stage.AutomationReadCache = true
		return runtimewiring.BuildDeterministicExecutor(stage)
	}
	return runtimewiring.BuildExecutorConfig(input)
}
