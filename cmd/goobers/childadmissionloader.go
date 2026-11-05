package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/runtimewiring"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// loadPinnedChildStage is shared by run-bound tool authority and queued child
// execution. The caller supplies an immutable, successfully APPLIED currentSet
// under its config/authority lock. Pending edits on disk are never authority.
// Credentials remain in normal execution wiring; this loader performs offline
// harness admission only and never revives an expired stage tool grant.
func loadPinnedChildStage(ctx context.Context, layout instance.Layout, cfg *instance.Config, currentSet *instance.ConfigSet, parent journal.RunIdentity, stage string) (childworkflow.Authority, error) {
	if cfg == nil || currentSet == nil || parent.Child != nil || parent.ConfigGeneration == "" || parent.Gaggle == "" || parent.Workflow == "" {
		return childworkflow.Authority{}, childworkflow.ErrAuthorityUnavailable
	}
	matches, err := instance.RootIdentityMatches(layout.Root, parent.InstanceID)
	if err != nil || !matches {
		return childworkflow.Authority{}, fmt.Errorf("child parent instance identity unavailable or mismatched: %w", errors.Join(childworkflow.ErrAuthorityUnavailable, err))
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		return childworkflow.Authority{}, err
	}
	return childworkflow.PinnedAuthority(ctx, localGenerationSource{store: store}, cfg, currentSet, parent, stage, childworkflow.OfflineGooberAdmission)
}

// localGenerationSource serves the daemon's retained execution generations.
type localGenerationSource struct{ store configgeneration.Store }

func (s localGenerationSource) Acquire(ctx context.Context, generation string) (childworkflow.Generation, error) {
	directory, lease, err := s.store.Acquire(ctx, generation)
	if err != nil {
		return childworkflow.Generation{}, err
	}
	set, report, err := loadConfigDirectory(directory)
	if err != nil {
		_ = lease.Release()
		return childworkflow.Generation{}, fmt.Errorf("load child parent generation: %w (%s)", err, runtimewiring.ValidationIssueSummary(report))
	}
	return childworkflow.Generation{Directory: directory, Set: set, Release: lease.Release}, nil
}

func childStageCatalog(cfg *instance.Config, source *instance.ConfigSet, parent childworkflow.ParentSelection, backend childworkflow.Backend) (childworkflow.AdmissionContext, *workflow.Machine, error) {
	return childworkflow.StageCatalog(cfg, source, parent, backend, childworkflow.OfflineGooberAdmission)
}
