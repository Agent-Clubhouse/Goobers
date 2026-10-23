package main

import (
	"fmt"
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
		executor, err := runtimewiring.BuildDeterministicExecutor(stage)
		if err != nil {
			return nil, err
		}
		if input.ExecutionFence != nil {
			return runtimewiring.ClaimFencedDeterministic{Deterministic: executor, Start: input.ExecutionFence}, nil
		}
		return executor, nil
	}
	input.BuildAgentic = func(stage runtimewiring.AgenticExecutorInput) (invoke.Goober, error) {
		executor, err := runtimewiring.BuildAgenticExecutor(stage)
		if err != nil {
			return nil, err
		}
		if input.ExecutionFence != nil {
			return runtimewiring.ClaimFencedGoober{Goober: executor, Start: input.ExecutionFence}, nil
		}
		return executor, nil
	}
	return runtimewiring.BuildRunnerConfig(input)
}
func deterministicStageConfigDigest(configDir, gaggle string) (string, error) {
	digest, err := runtimewiring.ConfigDirectoryDigestForGaggle(configDir, gaggle)
	if err != nil {
		return "", fmt.Errorf("digest deterministic-stage config: %w", err)
	}
	return digest, nil
}
