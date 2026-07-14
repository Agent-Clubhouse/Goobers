package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FakeAdapter is a scripted harness adapter: it runs no subprocess and needs
// no network or installed CLI, so it is both the deterministic-test double
// and the conformance-fixture harness (issue #19 acceptance: "a scripted
// fake-harness stage round-trips envelope → work in worktree → valid result
// envelope"; live LLM output is structural-only per §3.3, so fixture runs
// substitute FakeAdapter for the real Copilot CLI).
//
// FakeAdapter exercises the exact same completion-file read path a real
// adapter uses (readCompletion): Act simulates whatever side effect the real
// harness would have had in the workspace (typically writing the completion
// file), and Run then reads it back the same way CopilotAdapter does. A test
// that wants the fail-closed "harness never finished" path simply supplies an
// Act that writes nothing.
type FakeAdapter struct {
	// AdapterName is returned by Name(); defaults to "fake" if empty.
	AdapterName string
	// Act simulates the harness's work against req.Workspace — e.g. writing
	// req.CompletionPath. A nil Act writes nothing (exercises ErrNoCompletion).
	Act func(ctx context.Context, req RunRequest) error
	// Transcript is returned verbatim as the session's captured transcript.
	Transcript []byte
	// TranscriptTruncated, if set, is returned verbatim on Outcome — lets
	// tests simulate a real subprocess-based adapter's transcript cap (#245)
	// without generating enough output to actually hit it.
	TranscriptTruncated bool
	// TranscriptDroppedBytes is returned verbatim on Outcome alongside
	// TranscriptTruncated.
	TranscriptDroppedBytes int64
	// PreflightErr, if set, is returned by Preflight — lets tests simulate a
	// harness that isn't installed/signed in.
	PreflightErr error
}

// Name returns the adapter's registry name.
func (f *FakeAdapter) Name() string {
	if f.AdapterName != "" {
		return f.AdapterName
	}
	return "fake"
}

// Preflight returns PreflightErr (nil by default — the fake is always "ready").
func (f *FakeAdapter) Preflight(ctx context.Context) error {
	return f.PreflightErr
}

// Run simulates one harness session: invoke Act (if set) against the
// workspace, then read back whatever completion file resulted.
func (f *FakeAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	out := Outcome{Transcript: f.Transcript, TranscriptTruncated: f.TranscriptTruncated, TranscriptDroppedBytes: f.TranscriptDroppedBytes}
	if f.Act != nil {
		if err := f.Act(ctx, req); err != nil {
			return out, err
		}
	}
	payload, err := readCompletion(req.Workspace, req.CompletionPath)
	if err != nil {
		return out, err
	}
	out.Payload = payload
	return out, nil
}

// WriteCompletion marshals v as JSON and writes it to workspace/relPath,
// creating parent directories as needed — the shape a FakeAdapter.Act (or an
// e2e fixture harness) uses to simulate a real harness writing its result or
// verdict completion file.
func WriteCompletion(workspace, relPath string, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("harness: marshal completion payload: %w", err)
	}
	full := filepath.Join(workspace, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("harness: create completion dir: %w", err)
	}
	if err := os.WriteFile(full, b, 0o644); err != nil {
		return fmt.Errorf("harness: write completion file: %w", err)
	}
	return nil
}
