package engine

import "context"

// SpanSource loads an executor-produced span by digest.
type SpanSource interface {
	Get(ctx context.Context, digest string) ([]byte, error)
}
