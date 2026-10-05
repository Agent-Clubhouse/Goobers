package worktree

import (
	"context"
	"errors"
)

type gitExecutionKey struct{}

// GitExecution binds a trusted run's Git identity without replacing the shared
// Manager or its repository locks. Environment handles every remote; Prepare
// handles every command, including local commands which could invoke helpers.
// Neither callback may return ambient credentials for an unmatched route.
type GitExecution struct {
	Environment func(context.Context, string) ([]string, error)
	Prepare     func(context.Context, []string) (context.Context, []string, func(), error)
}

// WithGitExecution installs host-owned execution authority; workflow input must
// never be used to construct it. It survives runner drain-context detachment.
func WithGitExecution(ctx context.Context, execution GitExecution) context.Context {
	return context.WithValue(ctx, gitExecutionKey{}, execution)
}

func (m *Manager) executionGitEnvironment(ctx context.Context, remote string) ([]string, error) {
	if execution, ok := ctx.Value(gitExecutionKey{}).(GitExecution); ok {
		if execution.Environment == nil {
			return nil, errors.New("worktree: trusted Git identity is incomplete")
		}
		return execution.Environment(ctx, remote)
	}
	if m.gitEnv != nil {
		return m.gitEnv(ctx, remote)
	}
	return nil, nil
}

func prepareExecutionGit(ctx context.Context, env []string) (context.Context, []string, func(), error) {
	if execution, ok := ctx.Value(gitExecutionKey{}).(GitExecution); ok {
		if execution.Prepare == nil {
			return nil, nil, nil, errors.New("worktree: trusted Git execution is incomplete")
		}
		return execution.Prepare(ctx, env)
	}
	return ctx, env, func() {}, nil
}
