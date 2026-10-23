package main

import (
	"os/exec"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/worktree"
)

type runnerCompositionInput = runtimewiring.RunnerCompositionInput

var runnerLookPath = exec.LookPath

func buildRunnerConfig(input runnerCompositionInput) (runner.Config, *worktree.Manager, error) {
	binary, err := workerExecutorBinary()
	if err != nil {
		return runner.Config{}, nil, err
	}
	input.ExecutorBinary = binary
	input.Diagnostics, input.DiagnosticsMaxBytes = diagnosticsMode, diagnosticsMaxOutputBytes
	input.LookPath = runnerLookPath
	input.ConfigureWorktree = func(manager *worktree.Manager) error {
		cloneURL := runtimewiring.RepoCloneURL
		if cloneURL == nil {
			cloneURL = runner.DefaultRepoCloneURL
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
	return runtimewiring.BuildRunnerConfig(input)
}
