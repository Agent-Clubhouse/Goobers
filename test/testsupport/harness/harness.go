// Package harness provides shared test doubles for the agent runtime harness.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	runtimeharness "github.com/goobers/goobers/internal/harness"
)

// FakeAdapter is a configurable in-process runtime harness adapter.
type FakeAdapter struct {
	AdapterName            string
	Act                    func(context.Context, runtimeharness.RunRequest) error
	Transcript             []byte
	TranscriptTruncated    bool
	TranscriptDroppedBytes int64
	Stderr                 []byte
	PreflightErr           error
	Version                string
}

// Name returns the configured adapter name, or "fake" when none is configured.
func (f *FakeAdapter) Name() string {
	if f.AdapterName != "" {
		return f.AdapterName
	}
	return "fake"
}

// Preflight returns the configured version and preflight error.
func (f *FakeAdapter) Preflight(context.Context) (runtimeharness.PreflightInfo, error) {
	if f.PreflightErr != nil {
		return runtimeharness.PreflightInfo{}, f.PreflightErr
	}
	version := f.Version
	if version == "" {
		version = f.Name()
	}
	return runtimeharness.PreflightInfo{Version: version}, nil
}

// Run invokes Act and reads its completion payload from the request workspace.
func (f *FakeAdapter) Run(ctx context.Context, req runtimeharness.RunRequest) (runtimeharness.Outcome, error) {
	out := runtimeharness.Outcome{
		Transcript:             f.Transcript,
		TranscriptTruncated:    f.TranscriptTruncated,
		TranscriptDroppedBytes: f.TranscriptDroppedBytes,
		Stderr:                 f.Stderr,
	}
	if f.Act != nil {
		if err := f.Act(ctx, req); err != nil {
			return out, err
		}
	}
	payload, err := os.ReadFile(filepath.Join(req.Workspace, req.CompletionPath))
	if err != nil {
		if os.IsNotExist(err) {
			return out, fmt.Errorf("%w: %s", runtimeharness.ErrNoCompletion, req.CompletionPath)
		}
		return out, fmt.Errorf("harness: read completion file %s: %w", req.CompletionPath, err)
	}
	out.Payload = payload
	return out, nil
}

// WriteCompletion marshals v to the requested workspace-relative path.
func WriteCompletion(workspace, relPath string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("harness: marshal completion payload: %w", err)
	}
	path := filepath.Join(workspace, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("harness: create completion dir: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("harness: write completion file: %w", err)
	}
	return nil
}
