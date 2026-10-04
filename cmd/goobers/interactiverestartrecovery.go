package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
)

func (r *daemonRunnerRegistry) setInteractiveGenerationResolver(resolve executionGenerationResolver) {
	r.mu.Lock()
	r.resolveInteractiveGeneration = resolve
	r.mu.Unlock()
}

// The continuation owns its immutable context and generation. Recovery must not
// prepare another restart, reread selected guidance or replenish its allowance.
// The builder is offline; execution reacquires a current human policy lease.
func interactiveGenerationResolver(layout instance.Layout, setup *schedulerSetup, build func(context.Context, journal.RunIdentity) (intervention.Execution, error)) executionGenerationResolver {
	return func(ctx context.Context, id journal.RunIdentity) (executionGenerationRuntime, error) {
		if setup == nil || setup.InteractiveAccess == nil || build == nil || !runner.IsStageRestart(id) {
			return executionGenerationRuntime{}, errors.New("interactive recovery unavailable")
		}
		reader, err := journal.OpenReadOnly(filepath.Join(layout.ForGaggle(id.Gaggle).RunsDir(), id.RunID))
		if err != nil {
			return executionGenerationRuntime{}, err
		}
		retained, err := reader.Identity()
		if err != nil || !reflect.DeepEqual(retained, id) {
			return executionGenerationRuntime{}, errors.New("interactive recovery identity differs from retained journal")
		}
		authority, err := interactiveaccess.LoadRestartAuthority(reader, id)
		if err != nil {
			return executionGenerationRuntime{}, err
		}
		var result executionGenerationRuntime
		err = setup.InteractiveAccess.WithRestartSources(ctx, authority.Principal(), id.Gaggle, interactiveaccess.RestartSourceRequest{}, func(ctx context.Context, sources interactiveaccess.RestartSources) error {
			if sources.Gaggle.Spec.Enabled != nil && !*sources.Gaggle.Spec.Enabled {
				return errors.New("interactive recovery gaggle is disabled")
			}
			current := setup.Interventions.Snapshot().machines[localscheduler.WorkflowIdentity{Gaggle: id.Gaggle, Workflow: id.Workflow}]
			if current == nil || (current.Def.Spec.Enabled != nil && !*current.Def.Spec.Enabled) {
				return errors.New("interactive recovery workflow is removed or disabled")
			}
			execution, err := build(ctx, id)
			if err != nil {
				return err
			}
			if execution.Runner == nil || execution.Machine == nil || execution.Machine.Digest() != id.WorkflowDigest || execution.GooberDigest != id.GooberDigest {
				return errors.New("interactive recovery execution differs from retained pins")
			}
			result = executionGenerationRuntime{runner: execution.Runner, machine: execution.Machine, gooberDigest: execution.GooberDigest, repoRef: execution.RepoRef}
			return nil
		})
		return result, err
	}
}

// A revoked human command must remain inspectable without preventing unrelated
// gaggles from starting. Its journal remains the recovery authority.
func deferInteractiveGenerationRecovery(ctx context.Context, log *journal.InstanceLog, id journal.RunIdentity) bool {
	if !runner.IsStageRestart(id) || ctx.Err() != nil {
		return false
	}
	if log != nil {
		log.AppendBestEffort(journal.Event{Type: journal.EventError, Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: id.RunID,
			Error: &journal.ErrorDetail{Code: "interactive_recovery_deferred", Message: "human restart recovery refused retained authority, current permissions or supported execution; inspect the continuation and restore its authority before retrying"},
		})
	}
	return true
}
