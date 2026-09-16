package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func podWorkspaceBranch() (*apiv1.WorkspaceBranchBinding, *dispatcher.WorkspaceCheckout, error) {
	raw := os.Getenv(dispatcher.EnvWorkspaceBranchBinding)
	if raw == "" {
		return nil, nil, nil
	}
	var binding *apiv1.WorkspaceBranchBinding
	if err := decodePodRevisionJSON(raw, &binding); err != nil {
		return nil, nil, err
	}
	if binding == nil {
		return nil, nil, podRevisionFailure(workspacerevision.CodeInvalid, "owned workspace binding must be an object", nil)
	}
	if err := binding.Validate(); err != nil {
		return nil, nil, err
	}
	if !apiv1.WorkspaceMode(os.Getenv(dispatcher.EnvStageWorkspace)).IsWritableRepo() {
		return binding, nil, nil
	}
	var checkout *dispatcher.WorkspaceCheckout
	if err := decodePodRevisionJSON(os.Getenv(dispatcher.EnvWorkspaceCheckout), &checkout); err != nil || checkout == nil {
		return nil, nil, podRevisionFailure(workspacerevision.CodeUnauthorized, "owned workspace requires configured checkout transport", err)
	}
	_, err := workspacebranch.Accept(nil, binding, &apiv1.WorkspaceRevision{CommitSHA: binding.StartingSHA},
		checkout.Repository, os.Getenv(executor.BranchNamespaceEnvVar), os.Getenv(dispatcher.EnvWorkflow),
		os.Getenv(dispatcher.EnvRunID), true, true)
	if err != nil {
		return nil, nil, err
	}
	if binding.Ref != "refs/heads/"+os.Getenv(dispatcher.EnvWorkspaceBranch) ||
		os.Getenv(dispatcher.EnvStageSyncBase) == "true" || os.Getenv(dispatcher.EnvWorkspaceRevision) != "" ||
		os.Getenv(dispatcher.EnvCheckoutCapability) != "" {
		return nil, nil, podRevisionFailure(workspacerevision.CodeConflict, "owned branch transport contradicts durable ownership", nil)
	}
	return binding, checkout, nil
}

func checkoutOwnedBranch(ctx context.Context, dir string, stderr io.Writer, creds []dispatcher.MintedCredential, binding *apiv1.WorkspaceBranchBinding, checkout dispatcher.WorkspaceCheckout) (err error) {
	var selected []dispatcher.MintedCredential
	for _, cred := range creds {
		if cred.Capability == dispatcher.WorkspaceBranchCheckoutCapability {
			selected = append(selected, cred)
		}
	}
	if len(selected) != 1 || (selected[0].Value == "") != selected[0].Anonymous {
		return podRevisionFailure(workspacerevision.CodeUnauthorized, "owned branch requires exactly one checkout-only read grant", nil)
	}
	selected[0].Capability = "repo:read"
	auth, err := checkoutGitAuthEnv(dir, selected)
	if err != nil {
		return err
	}
	if selected[0].Anonymous {
		auth = anonymousRevisionEnvironment()
	}
	home, err := os.MkdirTemp("", "goobers-owned-checkout-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(home)) }()
	auth = revisionHomeEnvironment(sterileRevisionEnvironment(auth), home)
	git := selectedRevisionGit{dir: dir, env: auth}
	url, err := checkoutCloneURL(checkout.Repository)
	if err != nil {
		return err
	}
	out, err := git.output(ctx, "-c", "http.followRedirects=false", "ls-remote", "--refs", "--", url, binding.Ref)
	if err != nil {
		return podRevisionFailure(workspacerevision.CodeAcquisition, "read owned branch tip", err)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[1] != binding.Ref || apiv1.ValidateCommitSHA(fields[0]) != nil {
		return podRevisionFailure(workspacerevision.CodeConflict, "owned branch is missing or substituted", nil)
	}
	tip := fields[0]
	selected[0].Capability = dispatcher.WorkspaceRevisionCheckoutCapability
	if err := checkoutSelectedRevision(ctx, dir, selected, &apiv1.WorkspaceRevision{Repository: binding.Repository, CommitSHA: tip}, checkout); err != nil {
		return err
	}
	if _, err := git.output(ctx, "--no-lazy-fetch", "merge-base", "--is-ancestor", binding.StartingSHA, tip); err != nil {
		return podRevisionFailure(workspacerevision.CodeConflict, "owned branch no longer descends from its starting SHA", err)
	}
	branch := strings.TrimPrefix(binding.Ref, "refs/heads/")
	if _, err := git.output(ctx, "checkout", "--quiet", "-b", branch, tip); err != nil {
		return err
	}
	base := os.Getenv(executor.BaseBranchEnvVar)
	if base == "" {
		base = "main"
	}
	// The legacy continuity helper composes safe.directory into slot 1;
	// explicitly reserve its expected credential-helper slot after stripping
	// all ambient config above.
	auth = append(auth, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=")
	return finishWritableRepoCheckoutOnExistingBranch(ctx, dir, auth, stderr, branch, base)
}
