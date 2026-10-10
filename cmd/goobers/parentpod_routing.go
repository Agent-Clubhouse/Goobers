package main

import (
	"context"
	"errors"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// The runner caches a goober across tasks and gates. Route each call using the
// pinned task opt-in, so sharing a profile never turns an ordinary gate into a
// parent pod. The outer claim fence still guards both execution paths.
func routeContainedParent(root, goober string, rec runner.ArtifactRecorder, ordinary invoke.Goober) (invoke.Goober, error) {
	routed := &parentRoutedGoober{root: root, goober: goober, ordinary: ordinary}
	owned, _, err := runner.OwnedJournalScope(rec)
	if err != nil {
		// Ordinary executor adapters may use an opaque recorder. Without owned
		// journal provenance the router grants no parent tasks at all.
		return routed, nil
	}
	routed.rec = owned
	reader, err := journal.OpenReadOnly(owned.Dir())
	if err != nil {
		return nil, err
	}
	id, err := reader.Identity()
	if err != nil {
		return nil, err
	}
	if id.Child != nil || id.WorkflowDigest == "" {
		return routed, nil
	}
	machine, err := runner.PinnedWorkflowMachine(reader, id)
	if err != nil {
		return nil, err
	}
	tasks := map[string]string{}
	for _, task := range machine.Def.Spec.Tasks {
		if task.ChildWorkflows != nil {
			tasks[id.RunID+":"+task.Name] = task.Goober
		}
	}
	if len(tasks) == 0 {
		return routed, nil
	}
	routed.tasks = tasks
	return routed, nil
}

type parentRoutedGoober struct {
	root, goober    string
	rec             runner.OwnedJournalRecorder
	ordinary        invoke.Goober
	ordinaryFactory func() (invoke.Goober, error)
	hasAssets       bool
	tasks           map[string]string
}

func (g *parentRoutedGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	selected := g.tasks[env.TaskID] != ""
	if selected != (env.ChildWorkflowOrigin != nil) || (selected && g.tasks[env.TaskID] != g.goober) {
		return apiv1.ResultEnvelope{}, errors.New("parent stage opt-in differs from pinned invocation")
	}
	if !selected {
		ordinary, err := g.ordinaryExecutor()
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		return ordinary.Invoke(ctx, env)
	}
	service, ok := stageGrantMinterFor(g.root).(*daemonCredentialService)
	if !ok || service.parentExecutors == nil {
		return apiv1.ResultEnvelope{}, childworkflow.ErrAuthorityUnavailable
	}
	parent, err := service.parentExecutors(g.goober, g.rec, nil)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return parent.Invoke(ctx, env)
}

func (g *parentRoutedGoober) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	if g.tasks[env.TaskID] != "" || env.ChildWorkflowOrigin != nil {
		return apiv1.Verdict{}, errors.New("parent task cannot route through a gate reviewer")
	}
	ordinary, err := g.ordinaryExecutor()
	if err != nil {
		return apiv1.Verdict{}, err
	}
	return ordinary.Review(ctx, env)
}

func (g *parentRoutedGoober) ordinaryExecutor() (invoke.Goober, error) {
	if g.ordinaryFactory != nil {
		return g.ordinaryFactory()
	}
	return g.ordinary, nil
}

func (g *parentRoutedGoober) HasAssetBundle() bool {
	if g.ordinaryFactory != nil {
		return g.hasAssets
	}
	assets, ok := g.ordinary.(interface{ HasAssetBundle() bool })
	return ok && assets.HasAssetBundle()
}

// Local construction stays eager for ordinary workflows. A pinned parent task
// does not construct a host harness or resolve its model credentials. Shared
// profiles still construct the guarded local executor when an ordinary task or
// gate actually uses it; a failed parent route never falls back to that path.
func bindParentRouting(root, goober string, rec runner.ArtifactRecorder, factory func() (invoke.Goober, error), hasAssets bool, fence executionFenceStart) (invoke.Goober, error) {
	routed, err := routeContainedParent(root, goober, rec, nil)
	if err != nil {
		return nil, err
	}
	parent := routed.(*parentRoutedGoober)
	if len(parent.tasks) == 0 {
		parent.ordinary, err = factory()
		if err != nil {
			return nil, err
		}
	} else {
		parent.ordinaryFactory = sync.OnceValues(factory)
		parent.hasAssets = hasAssets
	}
	return claimFencedGoober{Goober: routed, start: fence}, nil
}
