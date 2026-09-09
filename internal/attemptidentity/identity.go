package attemptidentity

import "context"

// Identity is the execution provenance Temporal exposes for one activity
// attempt. It is carried through the activity result or failure so the
// workflow can record it in its history-derived journal.
type Identity struct {
	BuildID        string `json:"buildId"`
	WorkerIdentity string `json:"workerIdentity"`
	TaskQueue      string `json:"taskQueue,omitempty"`
	ActivityID     string `json:"activityId,omitempty"`
	ActivityType   string `json:"activityType,omitempty"`
	Attempt        int32  `json:"attempt,omitempty"`
}

type contextKey struct{}

func WithContext(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, identity)
}

func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(contextKey{}).(Identity)
	return identity, ok
}
