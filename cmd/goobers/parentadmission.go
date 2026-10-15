package main

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

// Admission uses the runner's immutable archive and the currently installed
// authenticated daemon plane. Caller-supplied policy cannot substitute for the
// retained source. Stage dispatch independently rechecks current policy.
func containedParentAdmission(layout instance.Layout, generation string) func(context.Context, *workflow.Machine) error {
	return func(ctx context.Context, machine *workflow.Machine) error {
		service, ok := stageGrantMinterFor(layout.Root).(*daemonCredentialService)
		if !ok || generation == "" || service.children == nil || service.parentExecutors == nil || service.parentRecovery == nil || service.parentBorrowJournal == nil || machine.Def.Spec.Gaggle != layout.Gaggle() {
			return childworkflow.ErrAuthorityUnavailable
		}
		store, err := executionGenerationStore(layout)
		if err != nil {
			return err
		}
		directory, lease, err := store.Acquire(ctx, generation)
		if err != nil {
			return err
		}
		defer func() { _ = lease.Release() }()
		set, report, err := loadConfigDirectory(directory)
		if err != nil {
			return fmt.Errorf("parent archive: %w (%s)", err, validationIssueSummary(report))
		}
		var stage string
		for _, task := range machine.Def.Spec.Tasks {
			if task.ChildWorkflows != nil {
				stage = task.Name
				break
			}
		}
		_, pinned, err := childStageCatalog(service.config, set, childworkflow.ParentSelection{Gaggle: layout.Gaggle(), Workflow: machine.Def.Name, Stage: stage}, childworkflow.BackendRunner)
		if err != nil {
			return err
		}
		if pinned.Digest() != machine.Digest() {
			return childworkflow.ErrAuthorityUnavailable
		}
		_, err = containedParentSelection(service.config, set, pinned)
		return err
	}
}
