// Package providersnapshot carries one scheduler evaluation's provider-list
// snapshot identity through in-process consumers and built-in stage execution.
package providersnapshot

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// EnvVar carries the scheduler evaluation shared by provider-list consumers.
const EnvVar = "GOOBERS_PROVIDER_SNAPSHOT"

type contextKey struct{}

var sequence atomic.Uint64

// WithTick associates all provider reads started by one scheduler evaluation.
func WithTick(ctx context.Context, at time.Time) context.Context {
	if ID(ctx) != "" {
		return ctx
	}
	id := fmt.Sprintf("%s-%d", at.UTC().Format(time.RFC3339Nano), sequence.Add(1))
	return WithID(ctx, id)
}

// WithID associates an existing provider snapshot identifier with ctx.
func WithID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, id)
}

// ID returns the scheduler evaluation identifier associated with ctx.
func ID(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}
