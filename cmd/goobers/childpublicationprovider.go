package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

func (p *childStagePod) publicationTarget(ctx context.Context, authority childworkflow.Authority, stage, path string, fork recovery.ChildSnapshot) (childpublication.Target, error) {
	project := authority.Admission.Gaggle.Spec.Project
	defs := p.service.defs.Load()
	if defs == nil || !reflect.DeepEqual(project, p.runtime.repoRef) || !reflect.DeepEqual(defs.Scopes[p.identity.Gaggle].Project, project) {
		return childpublication.Target{}, errors.New("child publication repository differs from admitted and current gaggle")
	}
	remote, err := childRepoCloneURL(project)
	if err != nil {
		return childpublication.Target{}, err
	}
	base := project.Branch
	if base == "" {
		base = "main"
	}
	repository := providers.RepositoryRef{Provider: providers.ProviderKind(project.Provider), Owner: project.Owner, Project: project.Project, Name: project.Name, URL: project.BaseURL}
	return childpublication.Target{Child: p.start.Child, Identity: p.identity, Repository: repository, Remote: remote, Base: base, Head: providers.BranchNameIn(authority.Admission.Gaggle.Spec.BranchNamespace, "children", p.identity.RunID), Stage: stage, Workspace: path, Fork: fork}, nil
}

func (p *childStagePod) publicationProvider(target childpublication.Target, key string, credential httpapi.MintedCredential, scheme string) (childpublication.Publisher, error) {
	publisher := childpublication.Publisher{Queue: p.service.childQueue}
	if credential.Value == "" || (credential.ExpiresAt != nil && !credential.ExpiresAt.After(time.Now())) {
		return publisher, errors.New("child publication credential is missing or expired")
	}
	opts := []stageProviderOption{withStageProviderCapability(capability.Capability(key)), withStageProviderToken(credential.Value), withStageProviderRetriesDisabled()}
	var env []string
	switch target.Repository.Provider {
	case providers.ProviderGitHub:
		env = providers.GitHubGitAuthEnvironment(credential.Value, target.Remote, p.service.shared)
	case providers.ProviderADO:
		kind, err := adoCredentialKindForScheme(scheme)
		if err != nil {
			return publisher, err
		}
		source, err := providers.NewADODeliveredCredentialSourceWithExpiry(kind, credential.Value, key, credentialExpiry(credential))
		if err != nil {
			return publisher, err
		}
		opts = append(opts, withStageProviderADOCredential(source))
		env, err = providers.ADOGitAuthEnvironment(context.Background(), source, p.service.shared, target.Remote)
		if err != nil {
			return publisher, err
		}
	default:
		return publisher, errors.New("child publication currently supports GitHub and Azure DevOps")
	}
	publisher.Git = childpublication.GitCommand{Environment: env}
	if key == string(capability.RepoPush) {
		return publisher, nil
	}
	provider, err := newProviderForStage(p.service.layout.Root, target.Repository, false, opts...)
	if err != nil {
		return publisher, err
	}
	if ado, ok := provider.(*providers.ADOProvider); ok {
		providers.WithADOMaxRateLimitRetries(0)(ado)
	}
	attribution := providers.Attribution{Schema: 1, Goobers: true, InstanceID: p.identity.InstanceID, Gaggle: p.identity.Gaggle, Workflow: p.identity.Workflow, Task: target.Stage, Goober: "deterministic", Run: p.identity.RunID, Action: strings.Join([]string{"child-publication", p.identity.Child.ParentRunID, p.identity.Child.StageOccurrence}, ":")}
	if configured, ok := provider.(providers.AttributionConfigurer); ok {
		configured.SetAttribution(attribution)
	}
	reconciler, ok := provider.(childpublication.PRProvider)
	if !ok {
		return publisher, errors.New("child publication requires exact PR reconciliation")
	}
	publisher.PRs = reconciler
	return publisher, nil
}
