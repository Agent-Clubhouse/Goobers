package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
	// NestedAct simulates the harness's work for a nested-agent invocation
	// (RunNested). A nil NestedAct falls back to Act.
	NestedAct func(ctx context.Context, req RunRequest) error
	// Transcript is returned verbatim as the session's captured transcript.
	Transcript []byte
	// TranscriptTruncated, if set, is returned verbatim on Outcome — lets
	// tests simulate a real subprocess-based adapter's transcript cap (#245)
	// without generating enough output to actually hit it.
	TranscriptTruncated bool
	// TranscriptDroppedBytes is returned verbatim on Outcome alongside
	// TranscriptTruncated.
	TranscriptDroppedBytes int64
	// Stderr simulates separately captured subprocess stderr.
	Stderr []byte
	// PreflightErr, if set, is returned by Preflight — lets tests simulate a
	// harness that isn't installed/signed in.
	PreflightErr error
	// Version is returned by Preflight. Empty defaults to AdapterName.
	Version string
}

// Name returns the adapter's registry name.
func (f *FakeAdapter) Name() string {
	if f.AdapterName != "" {
		return f.AdapterName
	}
	return "fake"
}

// ValidateNestedAgentPolicy defers to the policy's own validation — the fake
// imposes no adapter-specific nested-agent restrictions.
func (f *FakeAdapter) ValidateNestedAgentPolicy(policy apiv1.NestedAgentPolicy) error {
	return policy.Validate()
}

// Preflight returns PreflightErr or the fake's deterministic version.
func (f *FakeAdapter) Preflight(ctx context.Context) (PreflightInfo, error) {
	if f.PreflightErr != nil {
		return PreflightInfo{}, f.PreflightErr
	}
	version := f.Version
	if version == "" {
		version = f.Name()
	}
	return PreflightInfo{Version: version}, nil
}

// Run simulates one top-level harness session: invoke Act (if set) against
// the workspace, then read back whatever completion file resulted.
func (f *FakeAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	if err := validateStandardExecution(req); err != nil {
		return Outcome{}, err
	}
	return f.run(ctx, req, f.Act)
}

// RunNested simulates a nested-agent harness session: invoke NestedAct (or
// Act if NestedAct is unset) against the workspace, then read back whatever
// completion file resulted.
func (f *FakeAdapter) RunNested(ctx context.Context, req RunRequest) (Outcome, error) {
	if err := validateNestedExecution(req); err != nil {
		return Outcome{}, err
	}
	act := f.NestedAct
	if act == nil {
		act = f.Act
	}
	return f.run(ctx, req, act)
}

func (f *FakeAdapter) run(ctx context.Context, req RunRequest, act func(context.Context, RunRequest) error) (Outcome, error) {
	out := Outcome{
		Transcript:             f.Transcript,
		TranscriptTruncated:    f.TranscriptTruncated,
		TranscriptDroppedBytes: f.TranscriptDroppedBytes,
		Stderr:                 f.Stderr,
	}
	if act != nil {
		if err := act(ctx, req); err != nil {
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
