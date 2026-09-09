package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

// Stage claims resolve policy from the trusted run pin before constructing a
// provider. Local-mode runs therefore need no shared-store credentials. The
// daemon's independent write-plane assembly must not use this environment-
// aware factory: its credentials belong to the daemon, not to a stage.
func stageSharedClaimResolver(layout instance.Layout) claimsclient.SharedClaimResolver {
	return stageClaimResolver{pinnedSharedClaimResolver{layout: layout, store: func(ctx context.Context, repo providers.RepositoryRef) (sharedclaim.Store, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		provider, err := newProviderForStageAs[*providers.GitHubProvider](layout.Root, repo, false,
			withStageProviderCapability(capability.RepoPush))
		if err != nil {
			return nil, err
		}
		return providers.GitHubSharedClaimStore{Provider: provider, Repository: repo}, nil
	}}}
}

type stageClaimResolver struct{ pinnedSharedClaimResolver }

func (r stageClaimResolver) Admission(ctx context.Context, key claimsclient.Key, runID, workflow string) (*claimsclient.SharedClaimBinding, error) {
	policyRunID := runID
	if owner, ok := parseBacklogReconcileRunID(runID); ok && workflow == "backlog-reconcile" {
		policyRunID = owner
	}
	directory, err := runDirFor(r.layout, policyRunID)
	if errors.Is(err, os.ErrNotExist) {
		// Historical local CLI claimers need not create a run journal. Preserve
		// that behavior only in a validated local-only instance. Never use
		// current configuration to override an existing run's pinned policy.
		if err := requireLocalOnlyClaimConfiguration(r.layout); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	legacy, err := legacyUnpinnedClaimRun(directory, policyRunID)
	if err != nil {
		return nil, err
	}
	if legacy {
		return nil, requireLocalOnlyClaimConfiguration(r.layout)
	}
	return r.pinnedSharedClaimResolver.Admission(ctx, key, runID, workflow)
}

func legacyUnpinnedClaimRun(directory, runID string) (bool, error) {
	reader, err := journal.OpenReadOnly(directory)
	if err != nil {
		return false, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return false, err
	}
	if identity.RunID != runID {
		return false, fmt.Errorf("claim journal identity does not match its run directory")
	}
	if identity.WorkflowDigest != "" {
		return false, nil
	}
	for _, input := range identity.Inputs {
		if input.Name == journal.PinnedWorkflowDefinitionInputName {
			return false, nil
		}
	}
	return true, nil
}

func requireLocalOnlyClaimConfiguration(layout instance.Layout) error {
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return fmt.Errorf("verify local-only claim configuration: %w (%s)", err, validationIssueSummary(report))
	}
	if set == nil {
		return fmt.Errorf("local-only claim configuration is unavailable")
	}
	for _, workflow := range set.Workflows {
		mode := workflow.Spec.Readiness.ClaimVisibility
		if mode != "" && mode != "local" {
			return fmt.Errorf("claim admission requires a run pin when shared workflows are configured")
		}
	}
	return nil
}
