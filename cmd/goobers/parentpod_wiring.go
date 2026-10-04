package main

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

type parentExecutorProvider func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error)

func withContainedParentExecutor(cfg runner.Config, root string, instanceConfig *instance.Config, definitions *instance.ConfigSet) runner.Config {
	cfg.ChildWorkflowRecoveryAdmission = verifyParentPodCustody
	base := cfg.NewAgentic
	cfg.NewAgentic = func(name string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
		ordinary, err := base(name, rec, reg)
		if err != nil {
			return nil, err
		}
		return parentPodRoute{Goober: ordinary, root: root, recorder: rec, registrar: reg}, nil
	}
	cfg.ChildWorkflowAdmission = func(machine *workflow.Machine) error {
		if cfg.PinnedWorkspace {
			return fmt.Errorf("%w: contained parent requires managed workspace custody", workflow.ErrChildWorkflowExecutionUnsupported)
		}
		if _, err := containedParentPlacements(instanceConfig, definitions, machine); err != nil {
			return err
		}
		s, ok := stageGrantMinterFor(root).(*daemonCredentialService)
		if !ok || s.parentExecutors == nil {
			return fmt.Errorf("%w: contained worker backend is unavailable", workflow.ErrChildWorkflowExecutionUnsupported)
		}
		return nil
	}
	return cfg
}

type parentPodRoute struct {
	invoke.Goober
	root      string
	recorder  runner.ArtifactRecorder
	registrar runner.SecretRegistrar
}

func (p parentPodRoute) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if env.ChildWorkflowOrigin == nil {
		return p.Goober.Invoke(ctx, env)
	}
	service, ok := stageGrantMinterFor(p.root).(*daemonCredentialService)
	if !ok || service.parentExecutors == nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("contained parent executor unavailable")
	}
	isolated, err := service.parentExecutors(p.recorder, p.registrar)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return isolated.Invoke(ctx, env)
}
