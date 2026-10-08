package main

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// providerAuthGates builds the pre-claim provider authorization gates (#5317)
// from the instance credential resolver the scheduler's counters already use.
type providerAuthGates struct {
	health *localscheduler.ProviderAuthHealth
}

func newProviderAuthGates(resolver credentials.Resolver, reg *journal.RegistryScrubber, revision string) providerAuthGates {
	var register func([]byte)
	if reg != nil {
		register = reg.Register
	}
	return providerAuthGates{health: localscheduler.NewProviderAuthHealth(revision, resolver.Resolve, register, verifyGitHubRepositoryReadAccess)}
}

// forWorkflow returns wf's gate, or nil when wf does not opt in. The credential
// ref follows the workflow's repository binding, matching the backlog counter.
func (g providerAuthGates) forWorkflow(cfg *instance.Config, wf *apiv1.Workflow, repoRef apiv1.RepoRef) localscheduler.ProviderAuthGate {
	if !wf.Spec.Readiness.RequireProviderAuthorization {
		return nil
	}
	return g.health.Gate(localscheduler.ProviderAuthTarget{
		Repository:    backlogCounterRepoRef(cfg, repoRef),
		CredentialRef: repoRef.Owner + "/" + repoRef.Name,
	})
}

func verifyGitHubRepositoryReadAccess(ctx context.Context, token string, repo providers.RepositoryRef) error {
	return newGitHubProvider(token, providers.WithMaxRateLimitRetries(0)).VerifyRepositoryReadAccess(ctx, repo)
}
