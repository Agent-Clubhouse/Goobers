package main

import (
	"context"
	"errors"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

type interactiveRestartCI struct {
	execution *interactiveRestartExecution
	rec       runner.ArtifactRecorder
	reg       runner.SecretRegistrar
}

func (e *interactiveRestartExecution) deterministic(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
	return interactiveRestartCI{execution: e, rec: rec, reg: reg}, nil
}
func (c interactiveRestartCI) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if env.Inputs[executor.InputKind] != executor.KindCIPoll || run.InjectRunContext || len(run.Env) > 0 || !slices.Contains(env.Capabilities, "provider:pr:write") {
		return apiv1.ResultEnvelope{}, errors.New("interactive restart supports only native CI polling deterministic effects")
	}
	ctx, release, err := interactiveOperationContext(ctx)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer release()
	runtime, err := interactiveRuntime(ctx)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	value, err := runtime.lease.Credential(ctx, "repository.read", interactiveaccess.Target{Kind: "repository", Repository: interactiveRepository(env.RepoRef)})
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	registrar := teeRegistrar{run: c.reg, shared: c.execution.setup.SharedRegistry}
	registrar.Register([]byte(value.Value))
	var poller executor.PRPoller
	switch env.RepoRef.Provider {
	case apiv1.ProviderGitHub:
		poller = providers.NewGitHubProvider(value.Value)
	case apiv1.ProviderADO:
		source, err := interactiveADOSource(value)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		poller = providers.NewADOProvider(env.RepoRef.Owner, env.RepoRef.Project, "", providers.WithADOCredentialSource(source), providers.WithADOSecretRegistrar(registrar))
	default:
		return apiv1.ResultEnvelope{}, errors.New("interactive CI provider unavailable")
	}
	delegate, err := executor.NewCIPollExecutor(poller, c.rec)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	config, err := executor.CIPollConfigFromEnvelope(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return delegate.Run(ctx, config)
}
