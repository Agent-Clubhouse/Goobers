package livejournal

import "context"

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
