package harness

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildLocalHarnessRefusesBeforeAdapterOrCredentials(t *testing.T) {
	rec := &fakeRecorder{}
	adapter := &FakeAdapter{Act: func(context.Context, RunRequest) error { t.Fatal("unisolated adapter invoked"); return nil }}
	injector := testInjector(t, "CHILD_PROVIDER_SECRET", "opaque-provider-token", noopRegistrar{})
	executor, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "work")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := credentials.WithChildCeiling(t.Context(), credentials.NewChildCeiling(false, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Invoke(ctx, testEnvelope(t.TempDir(), "repo:read"))
	if !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
		t.Fatalf("local child: %v", err)
	}
}
