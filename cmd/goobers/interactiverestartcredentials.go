package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

type interactiveRepositoryKey struct{}

func interactiveCapability(capability string) (apiv1.InteractiveAction, string, bool) {
	switch capability {
	case "agent:model", "journal:read":
		return "", "", true
	case "repo:read", "contents:read", "github:pr:read", "ado:code:read":
		return "repository.read", "repository", true
	case "repo:push", "provider:pr:write", "github:pr:write", "github:pr:review", "ado:pr:write", "ado:pr:comment", "ado:pr:status":
		return "pr.repair", "repository", true
	case "github:issues:read":
		return "backlog.read", "backlog", true
	case "github:issues:write", "github:milestones:write", "ado:work-items:write":
		return "backlog.edit", "backlog", true
	default:
		return "", "", false
	}
}

func (r interactiveCredentialResolver) Resolve(ctx context.Context, key string) (string, error) {
	runtime, err := interactiveRuntime(ctx)
	if err != nil {
		return "", err
	}
	if runtime.execution != r.execution {
		return "", errors.New("interactive credential execution mismatch")
	}
	if key == "agent:model" {
		resolve := r.execution.models[r.goober]
		if resolve == nil {
			return "", errors.New("interactive model API key is unavailable")
		}
		value, err := resolve(ctx)
		if err != nil {
			return "", errors.New("interactive model API key resolution failed")
		}
		spec := r.execution.goobers[r.goober]
		if !validInteractiveModelAPIKey(spec.Harness, value) {
			return "", errors.New("interactive restart requires a model API key, not an ambient or forge credential")
		}
		r.registrar.Register([]byte(value))
		return value, nil
	}
	action, kind, ok := interactiveCapability(key)
	if !ok || kind == "" {
		return "", errors.New("interactive credential capability is unsupported")
	}
	target := interactiveaccess.Target{Kind: kind}
	if kind == "repository" {
		repo, ok := ctx.Value(interactiveRepositoryKey{}).(apiv1.RepoRef)
		if !ok {
			return "", errors.New("interactive repository authority is missing")
		}
		target.Repository = interactiveRepository(repo)
	}
	value, err := runtime.lease.Credential(ctx, action, target)
	if err != nil {
		return "", err
	}
	r.registrar.Register([]byte(value.Value))
	return value.Value, nil
}

func validInteractiveModelAPIKey(kind apiv1.Harness, value string) bool {
	switch kind {
	case apiv1.HarnessClaudeCode:
		return strings.HasPrefix(value, "sk-ant-api")
	case apiv1.HarnessCodex:
		return strings.HasPrefix(value, "sk-") && !strings.HasPrefix(value, "sk-ant-")
	default:
		return false
	}
}

func interactiveRepository(repo apiv1.RepoRef) apiv1.InteractiveRepositoryIdentity {
	return apiv1.InteractiveRepositoryIdentity{Provider: repo.Provider, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
}

func (e *interactiveRestartExecution) repositoryIdentity(ctx context.Context, repo apiv1.RepoRef) (providers.RepositoryMetadata, error) {
	ctx, release, err := interactiveOperationContext(ctx)
	if err != nil {
		return providers.RepositoryMetadata{}, err
	}
	defer release()
	runtime, err := interactiveRuntime(ctx)
	if err != nil {
		return providers.RepositoryMetadata{}, err
	}
	credential, err := runtime.lease.Credential(ctx, "repository.read", interactiveaccess.Target{Kind: "repository", Repository: interactiveRepository(repo)})
	if err != nil {
		return providers.RepositoryMetadata{}, err
	}
	runtime.registrar.Register([]byte(credential.Value))
	ref := providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
	switch repo.Provider {
	case apiv1.ProviderGitHub:
		if repo.BaseURL != "" && strings.TrimRight(repo.BaseURL, "/") != "https://github.com" {
			return providers.RepositoryMetadata{}, errors.New("interactive restart custom GitHub endpoints are unavailable")
		}
		return providers.NewGitHubProvider(credential.Value).ReadRepository(ctx, ref)
	case apiv1.ProviderADO:
		source, err := interactiveADOSource(credential)
		if err != nil {
			return providers.RepositoryMetadata{}, err
		}
		return providers.NewADOProvider(repo.Owner, repo.Project, "", providers.WithADOCredentialSource(source), providers.WithADOSecretRegistrar(e.setup.SharedRegistry)).ReadRepository(ctx, ref)
	default:
		return providers.RepositoryMetadata{}, errors.New("interactive restart repository provider is unsupported")
	}
}

func interactiveADOSource(value interactiveaccess.Credential) (providers.ADOCredentialSource, error) {
	kind := "pat"
	if value.Scheme == "bearer" {
		kind = "bearer"
	} else if value.Scheme != "basic" {
		return nil, errors.New("interactive ADO credential scheme unavailable")
	}
	return providers.NewADODeliveredCredentialSourceWithExpiry(kind, value.Value, "interactive-restart", value.ExpiresAt)
}

func (r *interactiveRestartContext) gitEnvironment(ctx context.Context, remote string) ([]string, error) {
	repos := append([]apiv1.RepoRef{r.execution.gaggle.Spec.Project}, r.execution.gaggle.Spec.AdditionalRepos...)
	for _, repo := range repos {
		url, err := runner.DefaultRepoCloneURL(repo)
		if err != nil {
			return nil, err
		}
		if remote != url {
			continue
		}
		value, err := r.lease.Credential(ctx, "repository.read", interactiveaccess.Target{Kind: "repository", Repository: interactiveRepository(repo)})
		if err != nil {
			return nil, err
		}
		if repo.Provider == apiv1.ProviderGitHub {
			return providers.GitHubGitAuthEnvironment(value.Value, remote, r.registrar), nil
		}
		if repo.Provider == apiv1.ProviderADO {
			source, err := interactiveADOSource(value)
			if err != nil {
				return nil, err
			}
			return providers.ADOGitAuthEnvironment(ctx, source, r.execution.setup.SharedRegistry, remote)
		}
		return nil, errors.New("interactive Git provider unsupported")
	}
	return nil, errors.New("interactive Git target is outside the pinned gaggle")
}

func (r *interactiveRestartContext) prepareGit(ctx context.Context, provided []string) (context.Context, []string, func(), error) {
	scoped, release, err := interactiveOperationContext(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	env := procenv.IsolatedIdentityEnvironment(r.home)
	// Provider auth helpers include their ambient environment. Only their explicit
	// Git configuration entries are admitted into this clean human environment.
	hasConfig := false
	for _, entry := range provided {
		key, _, _ := strings.Cut(entry, "=")
		if key == "GIT_CONFIG_COUNT" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			env = append(env, entry)
			hasConfig = true
		}
	}
	if !hasConfig {
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=")
	}
	return scoped, env, release, nil
}

func interactiveCredentialInstructions() string {
	return fmt.Sprintln("This is a human-authorized restart. Use only the declared GOOBERS_CRED_<CAPABILITY> environment variables for provider operations; repository and backlog credentials are separate. Model credentials are solely for the configured model. Make repository changes through a pull request. Do not load host credentials or invoke instance-configured Goobers provider commands.")
}
