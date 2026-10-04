package main

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

// resolveGeneration uses the same retained source custody as initial dispatch.
// It never selects a generated workflow by its display name and never provisions
// a replacement workspace. The normal resume path verifies/adopts pinned custody.
func (l *queuedChildLauncher) resolveGeneration(ctx context.Context, id journal.RunIdentity) (executionGenerationRuntime, error) {
	if id.Child == nil || l.build == nil {
		return executionGenerationRuntime{}, childworkflow.ErrAuthorityUnavailable
	}
	ref, err := l.retainedChildIdentity(ctx, id)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	a, release, err := l.acquire(ctx, ref.Envelope)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	defer release()
	source, err := l.queue.ChildProposal(ctx, ref.Child.Identity)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	proposal, err := childworkflow.ValidateRetainedStart(a, ref.Envelope, source.Source)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	runtime, err := l.build(ctx, childExecutionStart{childExecutionRef: ref, Proposal: proposal})
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	defer runtime.release()
	if runtime.machine.Digest() != id.WorkflowDigest || runtime.gooberDigest != id.GooberDigest {
		return executionGenerationRuntime{}, errors.New("generated recovery runtime differs from journal pins")
	}
	// The already-retained journal and queue source remain durable generation
	// owners after this short construction lease ends.
	return runtime.executionGenerationRuntime, nil
}

func (r *daemonRunnerRegistry) setChildGenerationResolver(resolve executionGenerationResolver) {
	r.mu.Lock()
	r.resolveChildGeneration = resolve
	r.mu.Unlock()
}

// A generated child may lose current authority or retained custody while the
// daemon is down. Refusing that execution must not make unrelated gaggles
// unavailable. The durable queue/journal still pin the unresolved family; only
// its reconstructed runnable permit is released. Repair and a subsequent
// recovery pass are required: this does not silently choose another definition.
func deferChildGenerationRecovery(ctx context.Context, log *journal.InstanceLog, id journal.RunIdentity) bool {
	if id.Child == nil || ctx.Err() != nil {
		return false
	}
	if log != nil {
		log.AppendBestEffort(journal.Event{Type: journal.EventError, Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: id.RunID,
			Error: &journal.ErrorDetail{Code: "child_recovery_deferred", Message: "child recovery refused retained source, workspace custody, or current authority; repair the child admission and retry recovery, or cancel the family"},
		})
	}
	return true
}

// interruptedRuntimeInput groups the already-applied catalog with the exact
// retained-generation resolver. A historical child never consults the catalog.
type interruptedRuntimeInput struct {
	runner        *runner.Runner
	registry      *daemonRunnerRegistry
	machines      map[localscheduler.WorkflowIdentity]*workflow.Machine
	gooberDigests map[localscheduler.WorkflowIdentity]string
	repoRefs      map[localscheduler.WorkflowIdentity]apiv1.RepoRef
	log           *journal.InstanceLog
	release       func(string, string)
}

func resolveInterruptedRuntime(ctx context.Context, id journal.RunIdentity, in interruptedRuntimeInput) (executionGenerationRuntime, bool, error) {
	identity := localscheduler.WorkflowIdentity{Gaggle: id.Gaggle, Workflow: id.Workflow}
	machine, known := in.machines[identity]
	runtime := executionGenerationRuntime{runner: in.runner, machine: machine, gooberDigest: in.gooberDigests[identity], repoRef: in.repoRefs[identity]}
	if id.ConfigGeneration != "" || runner.IsStageRestart(id) {
		pinned, err := in.registry.executionGeneration(ctx, id)
		if err != nil {
			if !deferChildGenerationRecovery(ctx, in.log, id) && !deferInteractiveGenerationRecovery(ctx, in.log, id) {
				return executionGenerationRuntime{}, false, fmt.Errorf("resolve run %q execution generation: %w", id.RunID, err)
			}
			in.release(id.RunID, id.Workflow)
			return executionGenerationRuntime{}, false, nil
		}
		runtime, known = pinned, true
	}
	if runtime.runner == nil || !known {
		warnUnresolvableResume(in.log, id, runtime.runner == nil)
		return executionGenerationRuntime{}, false, nil
	}
	return runtime, true, nil
}
