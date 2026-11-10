package main

import (
	"context"
	"path/filepath"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workflowcluster"
)

// The daemon reuses its bound endpoint, signing key, queue and journal layout.
// Admission and policy reload ownership live in internal/childworkflow.
func (s *daemonCredentialService) enableChildWorkflows(queue *triggerqueue.Store, definitions *instance.ConfigSet) error {
	if s.grants == nil {
		return childworkflow.ErrAuthorityUnavailable
	}
	var clusterModel *workflowcluster.Model
	var clusterFitness map[string]workflowcluster.ClusterFitness
	clusterModelConfigured := s.config.WorkflowClustering != nil && s.config.WorkflowClustering.ModelPath != ""
	if clusterModelConfigured {
		settings := s.config.WorkflowClustering
		path := settings.ModelPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.layout.Root, path)
		}
		if model, loadErr := workflowcluster.LoadModel(path); loadErr == nil {
			clusterModel = &model
			if fitness, fitnessErr := workflowcluster.ConfiguredFitness(s.layout.Root, model); fitnessErr == nil {
				clusterFitness = make(map[string]workflowcluster.ClusterFitness, len(fitness.Clusters))
				for _, outcome := range fitness.Clusters {
					clusterFitness[outcome.ClusterID] = outcome
				}
			}
		}
	}
	runtime, err := childworkflow.NewRuntime(childworkflow.RuntimeConfig{
		Queue: queue, Grants: s.grants.Key(), Endpoint: s.grants.Endpoint(), Definitions: definitions,
		ClusterModel: clusterModel, ClusterModelConfigured: clusterModelConfigured, ClusterFitness: clusterFitness,
		OpenJournal: func(ctx context.Context, runID string) (*journal.Reader, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			dir, err := s.layout.FindRunDir(runID)
			if err != nil {
				return nil, err
			}
			return journal.OpenReadOnly(dir)
		},
		LoadPinnedStage: func(ctx context.Context, current *instance.ConfigSet, parent journal.RunIdentity, stage string) (childworkflow.PinnedStageAdmission, error) {
			return s.pinnedChildStage(ctx, current, parent, stage)
		},
	})
	if err != nil {
		return err
	}
	s.children = runtime
	s.childQueue = queue
	return nil
}

func childWorkflowAccessFor(root string, registrar runner.SecretRegistrar) harness.ChildWorkflowAccessProvider {
	// Lookup happens at invocation, after the API is bound and on every retry.
	// Ordinary CLI/worker invocations have no daemon issuer and fail closed.
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
		service, ok := stageGrantMinterFor(root).(*daemonCredentialService)
		if !ok || service.children == nil || registrar == nil || service.shared == nil {
			return nil, nil, childworkflow.ErrAuthorityUnavailable
		}
		return service.children.Acquire(ctx, env, runtimewiring.TeeRegistrar{Run: registrar, Shared: service.shared})
	}
}

func unregisterDaemonStageGrants(root string, service *daemonCredentialService) {
	if absolute, err := filepath.Abs(root); err == nil {
		stageGrantMinters.CompareAndDelete(absolute, service)
	}
}

func (r *configReloader) publishChildDefinitions(definitions *instance.ConfigSet, publish func() error) error {
	if r.setup.CredentialPlane == nil || r.setup.CredentialPlane.children == nil {
		return publish()
	}
	return r.setup.CredentialPlane.children.ApplyDefinitions(context.Background(), definitions, publish)
}
