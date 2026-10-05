package main

import (
	"context"
	"os"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// enterHumanRuntime is shared by accepted restarts and conversation turns. The
// caller owns the policy/source leases; uncertain process custody retains both.
func (e *interactiveRestartExecution) enterHumanRuntime(lease *interactiveaccess.ExecutionLease, reg runner.SecretRegistrar) (context.Context, func() error, error) {
	if err := lease.RequireSources(e.gaggle.Spec.Project, e.gaggle.Spec.Backlog, e.gaggle.Spec.AdditionalRepos); err != nil {
		return nil, nil, err
	}
	home, err := os.MkdirTemp("", "goobers-human-runtime-")
	if err != nil {
		return nil, nil, err
	}
	ctx, proof := invoke.WithWorkspaceQuiescence(lease.Context())
	runtime := &interactiveRestartContext{lease: lease, home: home, execution: e, registrar: teeRegistrar{run: reg, shared: e.setup.SharedRegistry}, proof: proof}
	ctx = context.WithValue(ctx, interactiveRestartContextKey{}, runtime)
	ctx = worktree.WithGitExecution(ctx, worktree.GitExecution{Environment: runtime.gitEnvironment, Prepare: runtime.prepareGit})
	closeRuntime := func() error {
		if err := proof.VerifyIdle(); err != nil {
			return err
		}
		return os.RemoveAll(home)
	}
	return ctx, closeRuntime, nil
}
