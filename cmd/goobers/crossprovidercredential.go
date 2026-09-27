package main

import (
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// refuseBacklogCredentialOnCodeProvider is the stage-provider seam's
// fail-closed guard for topology (b) (docs/design/ado-parity-dsl-2-0.md
// §7.2): a gaggle whose code is on Azure DevOps and whose backlog is on GitHub
// or Gitea has its backlog-family capabilities (backlogRoleCapabilities)
// bound to the backlog repository's credential. That credential must never be
// presented to Azure DevOps, so building an Azure DevOps provider from it is
// refused with an error that names the mismatch.
//
// The credential counts as backlog-family when the provider would resolve it
// from a backlog-family capability, or when an explicit token equals a
// credential delivered for one. Operator commands that authenticate with the
// repository's configured Azure DevOps auth never carry a stage credential and
// are not checked. Every gaggle that is not topology (b) is unaffected.
func refuseBacklogCredentialOnCodeProvider(cfg stageProviderConfig) error {
	if cfg.repo.Provider != providers.ProviderADO || cfg.configuredADOAuth {
		return nil
	}
	if !stageCredentialIsBacklogFamily(cfg) {
		return nil
	}
	backlog := backlogRepoRefForStage(cfg.root, cfg.repo)
	if !backlogOnOtherProvider(cfg.repo, backlog) {
		return nil
	}
	return fmt.Errorf(
		"refusing to open Azure DevOps repository %s with a backlog credential (%s): this gaggle's backlog is on %s, and github:issues:* and github:milestones:write credentials belong to that provider; address the backlog provider instead",
		repositoryDisplayName(cfg.repo), cfg.capability, backlog.Provider,
	)
}

// stageCredentialIsBacklogFamily reports whether the credential a stage
// provider would authenticate with belongs to the backlog capability family.
func stageCredentialIsBacklogFamily(cfg stageProviderConfig) bool {
	if cfg.token == "" {
		return isBacklogRoleCapability(cfg.capability)
	}
	for _, c := range backlogRoleCapabilities {
		if delivered := os.Getenv(executor.CredentialEnvVar(string(c))); delivered != "" && delivered == cfg.token {
			return true
		}
	}
	return false
}

// decompositionIssueRepo is the repository the decomposition stages
// (select-source, validate-plan, publish-batch) read, claim and publish
// backlog items in: the routed repository, exactly as before, unless the
// gaggle's backlog lives on another provider (topology (b)). Then it is the
// backlog repository, so those stages open the backlog provider with the
// github:issues:* credential bound to it and key the parent's claim by that
// provider.
func decompositionIssueRepo(root string) (providers.RepositoryRef, error) {
	repo, err := providerRepo(root)
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	return backlogProviderRepo(repo, backlogRepoRefForStage(root, repo)), nil
}

// isBacklogRoleCapability reports whether c is in the capability family that
// routes to a gaggle's backlog provider.
func isBacklogRoleCapability(c capability.Capability) bool {
	for _, backlog := range backlogRoleCapabilities {
		if c == backlog {
			return true
		}
	}
	return false
}
