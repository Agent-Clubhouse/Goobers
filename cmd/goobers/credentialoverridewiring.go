package main

import (
	"io"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/credentialoverride"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

// Wiring for internal/credentialoverride (#2744): hand it the runner's own
// grant materialization and the repository preflight seams.

// credentialOverrideRunner materializes override grants with the same helpers
// buildRoleCredentials uses.
func credentialOverrideRunner() credentialoverride.Runner {
	return credentialoverride.Runner{
		RepoCapabilities: repoCredentialedCapabilityNames(),
		StorageKey:       credentialGrantStorageKey,
		BacklogRole:      gaggleBacklogRole,
		FilterGrants:     withoutNonADORepoGrants,
		ProjectRepo:      configuredRepoForProject,
		MintsCredential:  adoRepositoryMintsCredential,
	}
}

func gaggleCredentialOverrideProbes(cfg *instance.Config, set *instance.ConfigSet) []credentialoverride.Probe {
	return credentialOverrideRunner().Probes(cfg, set)
}

// checkGaggleCredentialOverrideAccess is `validate --check-repos`' preflight
// of every gaggle's credentials: overrides; a failure is REPO004 at the entry.
func checkGaggleCredentialOverrideAccess(root, configFile string, cfg *instance.Config, set *instance.ConfigSet, stores credentials.StoreResolver, stdout io.Writer, diagnostics *diagnosticCollector) bool {
	file := diagnosticFile(root, configFile)
	prober := credentialoverride.Prober{
		Timeout:   repositoryPreflightTimeout,
		Reachable: targetRepositoryReachable,
		Size:      targetRepositorySize,
		Scrub:     scrubRepositoryError,
	}
	return prober.Check(cfg, gaggleCredentialOverrideProbes(cfg, set), stores, stdout, func(pointer, message string) {
		diagnostics.add(file, pointer, "REPO004", string(validate.Error), message)
	})
}
