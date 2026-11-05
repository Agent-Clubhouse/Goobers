package livejournal

import (
	"context"
	"fmt"
)

type requestBranchKey struct{}

// WithRequestBranch binds attribution from a host-verified contract. It is not
// part of the wire request. Only a daemon authority adapter should call it.
func WithRequestBranch(ctx context.Context, branch int) context.Context {
	return context.WithValue(ctx, requestBranchKey{}, branch)
}

func requestBranch(ctx context.Context) int {
	branch, _ := ctx.Value(requestBranchKey{}).(int)
	return branch
}

// opBranchContext accepts explicit host attribution without weakening a
// branch already verified by a custody adapter. Validate before batch writes.
func opBranchContext(ctx context.Context, op Op) (context.Context, error) {
	if op.Branch < 0 {
		return nil, fmt.Errorf("negative operation branch")
	}
	if op.Branch == 0 {
		return ctx, nil
	}
	if op.Kind != OpAppend && op.Kind != OpArtifact && op.Kind != OpSpan {
		return nil, fmt.Errorf("operation %q cannot override branch attribution", op.Kind)
	}
	if branch, scoped := ctx.Value(requestBranchKey{}).(int); scoped && branch != op.Branch {
		return nil, fmt.Errorf("operation branch differs from verified request branch")
	}
	if op.Event != nil && op.Event.Branch != 0 && op.Event.Branch != op.Branch {
		return nil, fmt.Errorf("event branch differs from operation branch")
	}
	return WithRequestBranch(ctx, op.Branch), nil
}

func artifactBranchKey(branch int, name string) string {
	return fmt.Sprintf("%d:%s", branch, name)
}
