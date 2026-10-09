package runner

import (
	"context"
	"encoding/json"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
)

// ParentForkSourceFunc is a host service, not a workflow-selected tool. A nil
// previous source captures the exclusively held root; a replay imports only
// that source's durable artifact and must return exactly the same identity.
type ParentForkSourceFunc func(context.Context, OwnedJournalRecorder, spec.Request, *spec.Source) (spec.Source, error)

func forkSourceRequest(in StartInput, started journal.Event) spec.Request {
	return spec.Request{RunID: in.RunID, Gaggle: in.Gaggle, Parallel: started.Parallel, Sequence: started.Seq, At: started.Time, Repository: in.RepoRef}
}

func (r *Runner) captureParallelForkSource(ctx context.Context, run *journal.Run, in StartInput, started journal.Event, branch string) (ParentForkPlan, error) {
	var empty ParentForkPlan
	if r.cfg.PrepareParentForkSource == nil {
		return empty, errors.New("parallel fork source service unavailable")
	}
	workspace, err := r.createStageWorkspace(ctx, in, "parallel-source", apiv1.WorkspaceRepo, false, branch)
	if err != nil {
		return empty, err
	}
	defer func() { _ = workspace.Remove(context.WithoutCancel(ctx)) }()
	request := forkSourceRequest(in, started)
	request.Workspace = workspace.path
	source, err := r.cfg.PrepareParentForkSource(ctx, run, request, nil)
	if err != nil {
		return empty, err
	}
	return ParentForkPlan{Version: 1, Parallel: started.Parallel, Sequence: started.Seq, RunID: in.RunID, Gaggle: in.Gaggle, Source: source}, nil
}

func recordParallelForkPlan(run *journal.Run, plan ParentForkPlan) (journal.Ref, error) {
	data, err := json.Marshal(plan)
	if err != nil {
		return journal.Ref{}, err
	}
	ref, err := run.RecordArtifactBoundedWithIntegrity("parallel-fork-plan.json", data, apiv1.IntegrityTrusted, maxParentForkPlanBytes)
	if err != nil {
		return ref, err
	}
	if ref.Digest != journal.Digest(data) {
		return ref, errors.New("parallel fork plan changed during recording")
	}
	err = run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Parallel: plan.Parallel, Runner: map[string]any{"kind": ParentForkPlannedKind, "plan": ref}})
	return ref, err
}
