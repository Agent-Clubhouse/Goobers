package harness

import (
	"context"
	"errors"
	"testing"

	runtimeharness "github.com/goobers/goobers/internal/harness"
)

func TestFakeAdapterReadsCompletion(t *testing.T) {
	workspace := t.TempDir()
	adapter := &FakeAdapter{Act: func(_ context.Context, req runtimeharness.RunRequest) error {
		return WriteCompletion(req.Workspace, req.CompletionPath, map[string]string{"status": "success"})
	}}
	outcome, err := adapter.Run(context.Background(), runtimeharness.RunRequest{
		Workspace:      workspace,
		CompletionPath: "completion.json",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(outcome.Payload), `{"status":"success"}`; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func TestFakeAdapterReportsMissingCompletion(t *testing.T) {
	adapter := &FakeAdapter{}
	_, err := adapter.Run(context.Background(), runtimeharness.RunRequest{
		Workspace:      t.TempDir(),
		CompletionPath: "missing.json",
	})
	if !errors.Is(err, runtimeharness.ErrNoCompletion) {
		t.Fatalf("Run error = %v, want ErrNoCompletion", err)
	}
}
