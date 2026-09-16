package main

import (
	"context"
	"errors"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/internal/worktree"
)

// workspaceBranchExecutor runs only inside the trusted host. Its grants and
// configured repositories are never derived from stage Inputs or local Git config.
type workspaceBranchExecutor struct {
	input    deterministicExecutorInput
	injector *credentials.Injector
}

func (e *workspaceBranchExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	refuse := func(message string) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{}, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: message}
	}
	if !slices.Contains(env.Capabilities, string(capability.RepoPush)) {
		return refuse("workspace branch operations require declared repo:push")
	}
	base := e.input.GaggleProject
	if env.WorkspaceRevision == nil {
		return refuse("workspace branch operations require an accepted selected revision")
	}
	source, err := workspacerevision.Resolve(*env.WorkspaceRevision, base, e.input.AdditionalRepos)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	binding, err := workspacebranch.Expected(base, env.WorkspaceRevision, env.BranchNamespace, env.WorkflowID, env.RunID)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if _, err := workspacebranch.Accept(env.WorkspaceBranchBinding, binding, env.WorkspaceRevision,
		base, env.BranchNamespace, env.WorkflowID, env.RunID, true, true); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	kind, _ := env.Inputs["kind"].(string)
	if kind != workspacebranch.KindEstablish && kind != workspacebranch.KindPublish {
		return refuse("unknown workspace branch operation")
	}
	if kind == workspacebranch.KindEstablish && (run.Workspace != apiv1.WorkspaceScratch || run.SyncBase) {
		return refuse("branch establishment must use a scratch workspace without syncBase")
	}
	cloneURL := repoCloneURL
	if cloneURL == nil {
		cloneURL = runner.DefaultRepoCloneURL
	}
	sourceURL, err := cloneURL(source)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	targetURL, err := cloneURL(base)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	read, err := e.access(ctx, source, string(capability.ContentsRead), false)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	write, err := e.access(ctx, base, string(capability.RepoPush), true)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	opts := worktree.RemoteBranchOptions{
		SourceURL: sourceURL, TargetURL: targetURL, Binding: *binding,
		SourceRead: read, TargetWrite: write, TempDir: e.input.ScratchDir,
	}
	if kind == workspacebranch.KindPublish {
		if env.WorkspaceBranchBinding == nil || !run.Workspace.IsWritableRepo() || run.SyncBase {
			return refuse("publication requires durable branch ownership and a writable workspace without syncBase")
		}
		err = worktree.PublishRemoteBranch(ctx, opts, env.Workspace)
	} else {
		err = worktree.EstablishRemoteBranch(ctx, opts)
	}
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
	if kind == workspacebranch.KindEstablish {
		result.WorkspaceBranchBinding = binding
		result.Outputs = map[string]any{runner.WorkspaceBranchOutput: strings.TrimPrefix(binding.Ref, "refs/heads/")}
	}
	return result, nil
}

func (e *workspaceBranchExecutor) access(ctx context.Context, repo apiv1.RepoRef, operation string, write bool) (worktree.RemoteBranchAccess, error) {
	owner := repo.Owner
	if repo.Provider == apiv1.ProviderADO {
		owner += "/" + repo.Project
	}
	if e.input.Config != nil {
		for _, configured := range e.input.Config.Repos {
			configuredOwner := configured.Owner
			if configured.Provider == string(apiv1.ProviderADO) {
				configuredOwner += "/" + configured.Project
			}
			if configuredOwner == owner && configured.Name == repo.Name &&
				(configured.Provider != string(repo.Provider) ||
					!strings.EqualFold(strings.TrimRight(configured.BaseURL, "/"), strings.TrimRight(repo.BaseURL, "/"))) {
				return worktree.RemoteBranchAccess{}, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized,
					Message: "repository-qualified branch grant is ambiguous across configured services"}
			}
		}
	}
	key := credentials.RepoScopedCapability(operation, owner, repo.Name)
	set, err := e.injector.MaterializeRestricted(ctx, []string{key})
	if err != nil {
		return worktree.RemoteBranchAccess{}, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "resolve repository-specific branch grant", Cause: err}
	}
	token, err := set.Token(ctx, key)
	if err == nil && token != "" {
		return worktree.RemoteBranchAccess{Authorized: true, Token: token}, nil
	}
	if !write && errors.Is(err, credentials.ErrNoCredentialForCapability) && !checkoutAuthenticationConfigured(e.input.Config, repo) {
		return worktree.RemoteBranchAccess{Authorized: true}, nil
	}
	return worktree.RemoteBranchAccess{}, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "missing repository-specific branch grant", Cause: err}
}
