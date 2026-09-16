package worktree

import (
	"context"
	"errors"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// RemoteBranchCleanupOutcome distinguishes deletion from evidence-preserving refusals.
type RemoteBranchCleanupOutcome string

// Cleanup outcomes are stable values recorded by the terminal coordinator.
const (
	CleanupDeleted          RemoteBranchCleanupOutcome = "workspace_branch_deleted"
	CleanupAlreadyAbsent    RemoteBranchCleanupOutcome = "workspace_branch_already_absent"
	CleanupTipChanged       RemoteBranchCleanupOutcome = "workspace_branch_tip_changed"
	CleanupOwnershipMissing RemoteBranchCleanupOutcome = "workspace_branch_ownership_missing"
	CleanupOwnershipInvalid RemoteBranchCleanupOutcome = "workspace_branch_ownership_invalid"
	CleanupFailed           RemoteBranchCleanupOutcome = "workspace_branch_cleanup_failed"
)

// RemoteBranchCleanupResult records the lease and observation without changing ownership.
type RemoteBranchCleanupResult struct {
	Outcome     RemoteBranchCleanupOutcome
	ExpectedSHA string
	ObservedSHA string
}

// CleanupRemoteBranch deletes only an exact, durably owned tip. A lost response
// is reconciled by observation; a changed tip is preserved, never retried with
// a newly observed SHA.
func CleanupRemoteBranch(ctx context.Context, opts RemoteBranchOptions, expectedSHA string) (result RemoteBranchCleanupResult, err error) {
	result = RemoteBranchCleanupResult{Outcome: CleanupOwnershipInvalid, ExpectedSHA: expectedSHA}
	if err := opts.Binding.Validate(); err != nil {
		return result, revisionFailure(workspacerevision.CodeInvalid, "cleanup ownership is invalid", err)
	}
	if expectedSHA == "" {
		result.Outcome = CleanupOwnershipMissing
		return result, nil
	}
	if err := apiv1.ValidateCommitSHA(expectedSHA); err != nil {
		return result, revisionFailure(workspacerevision.CodeInvalid, "cleanup expected tip is invalid", err)
	}
	result.Outcome = CleanupFailed
	if opts.TargetURL == "" || !opts.TargetWrite.Authorized {
		return result, revisionFailure(workspacerevision.CodeUnauthorized, "cleanup requires the configured target grant", nil)
	}
	dir, err := os.MkdirTemp(opts.TempDir, "goobers-branch-cleanup-")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	script, err := credentials.WriteAskpassScript(dir)
	if err != nil {
		return result, err
	}
	env := sterileBranchEnv(dir, script, opts.TargetWrite)
	run := func(args ...string) (string, error) { return remoteBranchGit(ctx, dir, env, args...) }
	format := "sha1"
	if len(expectedSHA) == 64 {
		format = "sha256"
	}
	if _, err := run("init", "--bare", "--template=", "--object-format="+format, "."); err != nil {
		return result, err
	}
	read := func() (string, error) {
		out, err := run("ls-remote", "--refs", "--", opts.TargetURL, opts.Binding.Ref)
		if err != nil || out == "" {
			return "", err
		}
		fields := strings.Fields(out)
		if len(fields) != 2 || fields[1] != opts.Binding.Ref || apiv1.ValidateCommitSHA(fields[0]) != nil {
			return "", revisionFailure(workspacerevision.CodeSHAMismatch, "cleanup remote returned a substituted ref", nil)
		}
		return fields[0], nil
	}
	result.ObservedSHA, err = read()
	if err != nil {
		return result, err
	}
	if result.ObservedSHA == "" {
		result.Outcome = CleanupAlreadyAbsent
		return result, nil
	}
	if result.ObservedSHA != expectedSHA {
		result.Outcome = CleanupTipChanged
		return result, nil
	}
	_, pushErr := run("push", "--porcelain", "--no-verify",
		"--force-with-lease="+opts.Binding.Ref+":"+expectedSHA, "--", opts.TargetURL, ":"+opts.Binding.Ref)
	result.ObservedSHA, err = read()
	if err != nil {
		return result, err
	}
	if result.ObservedSHA == "" {
		result.Outcome = CleanupDeleted
		return result, nil
	}
	if result.ObservedSHA != expectedSHA {
		result.Outcome = CleanupTipChanged
		return result, nil
	}
	return result, revisionFailure(workspacerevision.CodeAcquisition, "owned branch cleanup did not complete", pushErr)
}
