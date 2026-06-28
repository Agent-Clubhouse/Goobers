package operator

import (
	"context"
	"log/slog"
	"testing"
)

func TestNoopRegistrar(t *testing.T) {
	r := NoopRegistrar{Log: slog.Default()}
	if err := r.EnsureRegistered(context.Background(), "web", []string{"impl"}); err != nil {
		t.Fatalf("noop registrar should not error: %v", err)
	}
	// Empty workflow set + nil logger must also be safe.
	if err := (NoopRegistrar{}).EnsureRegistered(context.Background(), "web", nil); err != nil {
		t.Fatalf("noop registrar (no log) should not error: %v", err)
	}
}
