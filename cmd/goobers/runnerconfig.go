package main

import (
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runtimewiring"
)

// localRunnerConfig is the only conversion from shared executor construction
// to the local workflow driver. Cloud workers never construct this config.
func localRunnerConfig(shared runtimewiring.ExecutorConfig) runner.Config {
	return runner.Config{
		ConfigGeneration:          shared.ConfigGeneration,
		RunControls:               shared.RunControls,
		NewDeterministic:          shared.NewDeterministic,
		NewAgentic:                shared.NewAgentic,
		NewAgenticWithMCPServers:  shared.NewAgenticWithMCPServers,
		Automated:                 shared.Automated,
		RunnersDeclared:           shared.RunnersDeclared,
		Worktrees:                 shared.Worktrees,
		PinnedWorkspace:           shared.PinnedWorkspace,
		PinnedCleanPolicy:         shared.PinnedCleanPolicy,
		BranchNamespaces:          shared.BranchNamespaces,
		ScratchDir:                shared.ScratchDir,
		RunsDir:                   shared.RunsDir,
		HandoffSchemaLoader:       shared.HandoffSchemaLoader,
		RepoCloneURL:              shared.RepoCloneURL,
		AdditionalRepos:           shared.AdditionalRepos,
		ResolveRepositoryIdentity: shared.ResolveRepositoryIdentity,
		GateGooberCapabilities:    shared.GateGooberCapabilities,
		AgentProvenance:           shared.AgentProvenance,
		LookPathFunc:              shared.LookPathFunc,
		SelfExecutionDenied:       shared.SelfExecutionDenied,
		SelfExecutionObserved:     shared.SelfExecutionObserved,
		Telemetry:                 shared.Telemetry,
		IssueOwnershipAssignees:   shared.IssueOwnershipAssignees,
		IssueOwnershipUnassigned:  shared.IssueOwnershipUnassigned,
	}
}
