package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// writeRaw writes raw bytes to workspace/relPath, creating parent dirs —
// used by tests that need a malformed (non-JSON) completion file, unlike
// WriteCompletion which always marshals valid JSON.
func writeRaw(workspace, relPath, content string) error {
	full := filepath.Join(workspace, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

// fakeRecorder is an in-memory SpanRecorder + ArtifactRecorder +
// ContextResolver double — mirrors production, where the same *journal.Run
// satisfies all three structurally (with runsDir standing in for the
// instance-layout pairing harness.NewContextResolver does in production,
// #103/T3). dir backs ContextResolver's Dir(); tests that never populate
// ContextPointers can leave it empty, since materializeContext never calls
// Dir() when there's nothing to resolve. runsDir similarly can stay empty
// for any test that never sets ContextPointer.RunID.
type fakeRecorder struct {
	spans     []recordedSpan
	artifacts []recordedArtifact
	dir       string
	runsDir   string
	err       error
}

type recordedSpan struct {
	stage, name string
	schema      string
	data        []byte
}

type recordedArtifact struct {
	name string
	data []byte
}

func (f *fakeRecorder) RecordSpanWithSchema(stage, name, dataSchema string, data []byte) (journal.Ref, error) {
	if f.err != nil {
		return journal.Ref{}, f.err
	}
	f.spans = append(f.spans, recordedSpan{stage: stage, name: name, schema: dataSchema, data: append([]byte(nil), data...)})
	return journal.Ref{Digest: journal.Digest(data)}, nil
}

func (f *fakeRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	if f.err != nil {
		return journal.Ref{}, f.err
	}
	f.artifacts = append(f.artifacts, recordedArtifact{name: name, data: append([]byte(nil), data...)})
	return journal.Ref{Path: "artifacts/fake/" + name, Digest: journal.Digest(data), Size: int64(len(data))}, nil
}

func (f *fakeRecorder) Dir() string { return f.dir }

func (f *fakeRecorder) RunsDir() string { return f.runsDir }

func testInjector(t *testing.T, tokenEnv, envVal string, registrar credentials.SecretRegistrar) *credentials.Injector {
	t.Helper()
	if tokenEnv == "" {
		tokenEnv = "UNUSED_TOKEN_ENV"
		envVal = "unused"
	}
	t.Setenv(tokenEnv, envVal)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "repo-token", Env: tokenEnv},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{
		{Capability: "repo:read", Ref: "repo-token"},
	}, registrar)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	return injector
}

func testEnvelope(workspace string, capabilities ...string) apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{
		TaskID:       "implement",
		WorkflowID:   "default-implement",
		RunID:        "run-1",
		Gaggle:       "example",
		Goal:         "implement the thing",
		Workspace:    workspace,
		Capabilities: capabilities,
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	}
}

func TestExecutorInvokeRoundTrip(t *testing.T) {
	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Transcript: []byte("implementing... done"),
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "did the thing",
			})
		},
	}

	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "be a good coder")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(t.TempDir(), "repo:read")
	result, err := exec.Invoke(context.Background(), env)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("Status = %q, want success", result.Status)
	}
	if result.Summary != "did the thing" {
		t.Fatalf("Summary = %q", result.Summary)
	}
	if len(rec.spans) != 1 {
		t.Fatalf("want 1 recorded span, got %d", len(rec.spans))
	}
	if rec.spans[0].stage != "implement" {
		t.Fatalf("span stage = %q, want %q", rec.spans[0].stage, "implement")
	}
	if rec.spans[0].schema != telemetry.GenAIEventSchema {
		t.Fatalf("span schema = %q, want %q", rec.spans[0].schema, telemetry.GenAIEventSchema)
	}
	events := decodeTranscriptEvents(t, rec.spans[0].data)
	if len(events) != 2 {
		t.Fatalf("transcript events = %#v, want prompt and final output", events)
	}
	if events[0].Role != "user" || !strings.Contains(events[0].Content, "implement the thing") || !strings.Contains(events[0].Content, "be a good coder") {
		t.Fatalf("prompt event = %#v", events[0])
	}
	if events[1].Role != "assistant" || events[1].Content != "implementing... done" {
		t.Fatalf("final output event = %#v", events[1])
	}
}

func TestExecutorMaterializesAssetsBeforeInvocation(t *testing.T) {
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "templates", "review.md"), []byte("review carefully"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := gooberassets.Load(source)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
		data, err := os.ReadFile(filepath.Join(req.Workspace, gooberassets.WorkspaceDir, "templates", "review.md"))
		if err != nil {
			return err
		}
		if string(data) != "review carefully" {
			return fmt.Errorf("asset content = %q", data)
		}
		return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	}}
	rec := &fakeRecorder{}
	exec, err := NewExecutor(
		adapter,
		testInjector(t, "", "", noopRegistrar{}),
		rec,
		rec,
		rec,
		journal.NewPatternScrubber(),
		"",
		WithAssetBundle(bundle),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir())); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
}

func TestExecutorPassesHarnessConfig(t *testing.T) {
	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			if req.Model != "claude-sonnet-4.5" {
				return fmt.Errorf("model = %q", req.Model)
			}
			if got := string(req.HarnessOptions["reasoningEffort"].Raw); got != `"high"` {
				return fmt.Errorf("harness options = %#v", req.HarnessOptions)
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	exec, err := NewExecutor(
		adapter,
		testInjector(t, "", "", noopRegistrar{}),
		rec,
		rec,
		rec,
		journal.NewPatternScrubber(),
		"",
		WithHarnessConfig("claude-sonnet-4.5", map[string]apiextensionsv1.JSON{
			"reasoningEffort": {Raw: []byte(`"high"`)},
		}),
	)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	if _, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir())); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
}

func TestExecutorUsesInvocationTimeout(t *testing.T) {
	rec := &fakeRecorder{}
	var got time.Duration
	adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
		got = req.Timeout
		return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	}}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "", WithTimeout(time.Hour))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	env := testEnvelope(t.TempDir())
	env.Limits.MaxDurationSeconds = 45
	if _, err := exec.Invoke(context.Background(), env); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got != 45*time.Second {
		t.Fatalf("request timeout = %s, want 45s", got)
	}
}

// TestExecutorUsesConfiguredFallbackTimeout is the goober-level default-timeout
// path (#1070): when a stage declares no per-attempt limit, the session is
// bounded by the executor's configured timeout (which the runner wires from
// GooberSpec.TimeoutSeconds) rather than the built-in 30m default.
func TestExecutorUsesConfiguredFallbackTimeout(t *testing.T) {
	rec := &fakeRecorder{}
	var got time.Duration
	adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
		got = req.Timeout
		return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	}}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "", WithTimeout(90*time.Minute))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	env := testEnvelope(t.TempDir())
	// No env.Limits.MaxDurationSeconds — the stage sets no per-attempt timeout.
	if _, err := exec.Invoke(context.Background(), env); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got != 90*time.Minute {
		t.Fatalf("request timeout = %s, want 90m (the configured fallback, not the 30m built-in)", got)
	}
}

// TestExecutorMarksSessionTimeout is #724: a harness session timeout must
// surface across the invoke seam as invoke.IsTimeout so the runner can apply a
// stage's OnTimeout salvage policy without importing this package. A non-timeout
// adapter error must NOT be marked.
func TestExecutorMarksSessionTimeout(t *testing.T) {
	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return fmt.Errorf("copilot-cli: %w after 30m0s: copilot", ErrTimeout)
		},
	}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	_, err = exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err == nil {
		t.Fatal("Invoke: want an error on session timeout")
	}
	if !invoke.IsTimeout(err) {
		t.Fatalf("Invoke error %v not marked invoke.IsTimeout", err)
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Invoke error %v does not preserve ErrTimeout", err)
	}

	// A non-timeout adapter failure must not be misclassified as a timeout.
	other := &FakeAdapter{Act: func(ctx context.Context, req RunRequest) error {
		return errors.New("some other harness failure")
	}}
	exec2, err := NewExecutor(other, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	_, err = exec2.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err == nil || invoke.IsTimeout(err) {
		t.Fatalf("non-timeout error misclassified: %v", err)
	}
}

func TestExecutorInvokeFailsClosedOnMissingCompletion(t *testing.T) {
	adapter := &FakeAdapter{} // Act is nil: writes nothing
	injector := testInjector(t, "", "", noopRegistrar{})
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	_, err = exec.Invoke(context.Background(), testEnvelope(t.TempDir()))
	if !errors.Is(err, ErrNoCompletion) {
		t.Fatalf("Invoke error = %v, want ErrNoCompletion", err)
	}
}

func TestExecutorInvokeFailsClosedOnInvalidCompletion(t *testing.T) {
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			// Missing the required "status" field.
			return WriteCompletion(req.Workspace, req.CompletionPath, map[string]string{"summary": "nope"})
		},
	}
	injector := testInjector(t, "", "", noopRegistrar{})
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	_, err = exec.Invoke(context.Background(), testEnvelope(t.TempDir()))
	if !errors.Is(err, ErrInvalidCompletion) {
		t.Fatalf("Invoke error = %v, want ErrInvalidCompletion", err)
	}
}

func TestExecutorInvokeFailsClosedOnMalformedJSON(t *testing.T) {
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return writeRaw(req.Workspace, req.CompletionPath, "{not json")
		},
	}
	injector := testInjector(t, "", "", noopRegistrar{})
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	_, err = exec.Invoke(context.Background(), testEnvelope(t.TempDir()))
	if err == nil {
		t.Fatal("expected an error for malformed completion JSON")
	}
}

func TestExecutorCapabilityEnforcement(t *testing.T) {
	var gotErr error
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			// The stage tries to use a capability it never declared.
			_, gotErr = req.Credentials.Token(ctx, "repo:push")
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "REPO_TOKEN_ENV", "s3cr3t-token-value", noopRegistrar{})
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	// Declare only "repo:read" — "repo:push" was never declared for this call.
	_, err = exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !errors.Is(gotErr, credentials.ErrUndeclaredCapability) {
		t.Fatalf("Token(repo:push) error = %v, want ErrUndeclaredCapability", gotErr)
	}
}

func TestExecutorMaterializesDeclaredCapability(t *testing.T) {
	var gotToken string
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			var err error
			gotToken, err = req.Credentials.Token(ctx, "repo:read")
			if err != nil {
				return err
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "REPO_TOKEN_ENV", "s3cr3t-token-value", noopRegistrar{})
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	_, err = exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotToken != "s3cr3t-token-value" {
		t.Fatalf("token = %q, want the resolved secret", gotToken)
	}
}

func TestExecutorRecordsRedactedTranscript(t *testing.T) {
	reg, scrub := journal.DefaultScrubber()
	secret := "s3cr3t-token-value"

	// Negative control: this canary must be invisible to the pattern-net
	// alone, so redaction below can only be the registry scrubber's doing —
	// without this, a canary the pattern-net also happens to catch would
	// pass whether or not the registry path works at all (the false-assurance
	// failure mode flagged on #66/#81).
	if scrubbed := journal.NewPatternScrubber().Scrub([]byte(secret)); string(scrubbed) != secret {
		t.Fatalf("test canary %q is pattern-net-visible (%q) — pick an opaque value only the registry scrubber can redact", secret, scrubbed)
	}

	adapter := &FakeAdapter{
		Transcript: []byte("logging in with token=" + secret + " now"),
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "REPO_TOKEN_ENV", secret, reg)
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, scrub, "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	_, err = exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(rec.spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(rec.spans))
	}
	if strings.Contains(string(rec.spans[0].data), secret) {
		t.Fatalf("recorded span still contains the raw secret: %q", rec.spans[0].data)
	}
	if !strings.Contains(string(rec.spans[0].data), journal.Redacted) {
		t.Fatalf("recorded span missing redaction placeholder: %q", rec.spans[0].data)
	}
}

// TestExecutorInvokeSurfacesTranscriptTruncation is #245's Executor-level
// regression: when the Adapter reports its transcript was capped, Invoke
// surfaces that as a scalar ResultEnvelope output (mirroring
// internal/executor.ShellExecutor's stdoutTruncated/stderrTruncated), not
// just silently inside the recorded span.
func TestExecutorInvokeSurfacesTranscriptTruncation(t *testing.T) {
	adapter := &FakeAdapter{
		Transcript:             []byte("...\n[transcript truncated: 999 bytes dropped]\n"),
		TranscriptTruncated:    true,
		TranscriptDroppedBytes: 999,
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "REPO_TOKEN_ENV", "unused", noopRegistrar{})
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if truncated, _ := result.Outputs["transcriptTruncated"].(bool); !truncated {
		t.Fatalf("Outputs[transcriptTruncated] = %v, want true", result.Outputs["transcriptTruncated"])
	}
	if dropped, _ := result.Outputs["transcriptDroppedBytes"].(float64); dropped != 999 {
		t.Fatalf("Outputs[transcriptDroppedBytes] = %v, want 999", result.Outputs["transcriptDroppedBytes"])
	}
	events := decodeTranscriptEvents(t, rec.spans[0].data)
	if len(events) != 2 || !events[1].Truncated {
		t.Fatalf("truncated transcript events = %#v", events)
	}
}

func TestExecutorBoundsComposedCanonicalTranscript(t *testing.T) {
	const limit = int64(1024)
	adapter := &FakeAdapter{
		Transcript:             bytes.Repeat([]byte("x"), int(limit)),
		TranscriptTruncated:    true,
		TranscriptDroppedBytes: 77,
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "REPO_TOKEN_ENV", "unused", noopRegistrar{})
	rec := &fakeRecorder{}
	exec, err := NewExecutor(
		adapter,
		injector,
		rec,
		rec,
		rec,
		journal.NewPatternScrubber(),
		"",
		WithTranscriptLimit(limit),
	)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if truncated, _ := result.Outputs["transcriptTruncated"].(bool); !truncated {
		t.Fatalf("Outputs[transcriptTruncated] = %v, want true", result.Outputs["transcriptTruncated"])
	}
	dropped, _ := result.Outputs["transcriptDroppedBytes"].(float64)
	if dropped <= float64(adapter.TranscriptDroppedBytes) {
		t.Fatalf("Outputs[transcriptDroppedBytes] = %v, want > %d", result.Outputs["transcriptDroppedBytes"], adapter.TranscriptDroppedBytes)
	}
	events := decodeTranscriptEvents(t, rec.spans[0].data)
	marker := events[len(events)-1]
	if marker.Role != "system" || !marker.Truncated || !strings.Contains(marker.Content, fmt.Sprintf("%.0f bytes dropped", dropped)) {
		t.Fatalf("truncation marker = %#v", marker)
	}
	encodedMarker, err := marshalTranscriptEvents(marker)
	if err != nil {
		t.Fatalf("marshal truncation marker: %v", err)
	}
	if retained := len(rec.spans[0].data) - len(encodedMarker); retained > int(limit) {
		t.Fatalf("retained transcript bytes = %d, want at most %d", retained, limit)
	}
}

func TestExecutorInvokeLiftsDeclaredArtifactFile(t *testing.T) {
	const relPath = "output/diff.patch"
	const content = "--- a/file\n+++ b/file\n"

	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			if err := os.MkdirAll(filepath.Join(req.Workspace, filepath.Dir(relPath)), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(req.Workspace, relPath), []byte(content), 0o644); err != nil {
				return err
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(t.TempDir())
	env.Inputs = map[string]interface{}{InputArtifactFile: relPath}
	result, err := exec.Invoke(context.Background(), env)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("Status = %q, want success", result.Status)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %+v", len(result.Artifacts), result.Artifacts)
	}
	wantDigest := journal.Digest([]byte(content))
	if result.Artifacts[0].Digest != wantDigest {
		t.Fatalf("artifact digest = %q, want %q (digest-verified against the declared file's actual bytes)", result.Artifacts[0].Digest, wantDigest)
	}
	if len(rec.artifacts) != 1 || string(rec.artifacts[0].data) != content {
		t.Fatalf("recorder did not receive the declared file's bytes: %+v", rec.artifacts)
	}
}

func TestExecutorInvokeFailsClosedOnMissingDeclaredArtifactFile(t *testing.T) {
	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			// Declares InputArtifactFile but never writes it.
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "claims success",
			})
		},
	}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(t.TempDir())
	env.Inputs = map[string]interface{}{InputArtifactFile: "output/diff.patch"}
	result, err := exec.Invoke(context.Background(), env)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("Status = %q, want failure (a harness-claimed success cannot override a missing declared artifact)", result.Status)
	}
	if result.Error == nil || result.Error.Code != "missing_declared_artifact" {
		t.Fatalf("Error = %+v, want code missing_declared_artifact", result.Error)
	}
	if len(rec.artifacts) != 0 {
		t.Fatalf("expected no artifact recorded, got %d", len(rec.artifacts))
	}
}

// TestExecutorInvokeFailsClosedOnArtifactFilePathTraversal is the regression
// test for #120: a declared InputArtifactFile that lexically escapes the
// workspace (via "..") must fail the stage closed, never lift the escaped
// file's content into a recorded artifact — even when the harness itself
// claims success.
func TestExecutorInvokeFailsClosedOnArtifactFilePathTraversal(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := []byte("leaked-secret-content")
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), secret, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(workspace)
	env.Inputs = map[string]interface{}{InputArtifactFile: "../secret.txt"}
	result, err := exec.Invoke(context.Background(), env)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("Status = %q, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "declared_artifact_path_escape" {
		t.Fatalf("Error = %+v, want code declared_artifact_path_escape", result.Error)
	}
	if len(rec.artifacts) != 0 {
		t.Fatalf("expected no artifact recorded, got %+v", rec.artifacts)
	}
}

// TestExecutorInvokeFailsClosedOnArtifactFileSymlinkEscape is the symlink
// half of #120: a declared InputArtifactFile name that is lexically
// contained but is itself a symlink to a file outside the workspace must
// also fail the stage closed.
func TestExecutorInvokeFailsClosedOnArtifactFileSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	secret := []byte("leaked-secret-content")
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(workspace, "artifact.txt")); err != nil {
		t.Fatal(err)
	}

	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(workspace)
	env.Inputs = map[string]interface{}{InputArtifactFile: "artifact.txt"}
	result, err := exec.Invoke(context.Background(), env)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("Status = %q, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "declared_artifact_path_escape" {
		t.Fatalf("Error = %+v, want code declared_artifact_path_escape", result.Error)
	}
	if len(rec.artifacts) != 0 {
		t.Fatalf("expected no artifact recorded, got %+v", rec.artifacts)
	}
}

func TestExecutorReviewLiftsDeclaredArtifactFileAsEvidence(t *testing.T) {
	const relPath = "evidence/test.log"
	const content = "all tests passed\n"

	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			if err := os.MkdirAll(filepath.Join(req.Workspace, filepath.Dir(relPath)), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(req.Workspace, relPath), []byte(content), 0o644); err != nil {
				return err
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.Verdict{Decision: apiv1.VerdictPass})
		},
	}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(t.TempDir())
	env.Inputs = map[string]interface{}{InputArtifactFile: relPath}
	verdict, err := exec.Review(context.Background(), env)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Decision != apiv1.VerdictPass {
		t.Fatalf("Decision = %q, want pass", verdict.Decision)
	}
	if len(verdict.Evidence) != 1 || verdict.Evidence[0].Digest != journal.Digest([]byte(content)) {
		t.Fatalf("Evidence = %+v, want 1 pointer digest-verified against the declared file", verdict.Evidence)
	}
}

func TestExecutorScrubsDeclaredArtifactFileBeforeRecording(t *testing.T) {
	reg, scrub := journal.DefaultScrubber()
	secret := "s3cr3t-token-value"

	// Negative control (established on #66/#81/#85): this canary must be
	// invisible to the pattern-net alone, so any redaction below can only be
	// the registry scrubber's doing.
	if scrubbed := journal.NewPatternScrubber().Scrub([]byte(secret)); string(scrubbed) != secret {
		t.Fatalf("test canary %q is pattern-net-visible (%q) — pick an opaque value only the registry scrubber can redact", secret, scrubbed)
	}

	const relPath = "output/env.dump"
	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			content := "token=" + secret + "\n"
			if err := os.MkdirAll(filepath.Join(req.Workspace, filepath.Dir(relPath)), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(req.Workspace, relPath), []byte(content), 0o644); err != nil {
				return err
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "REPO_TOKEN_ENV", secret, reg)
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, scrub, "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(t.TempDir(), "repo:read")
	env.Inputs = map[string]interface{}{InputArtifactFile: relPath}
	if _, err := exec.Invoke(context.Background(), env); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(rec.artifacts) != 1 {
		t.Fatalf("want 1 recorded artifact, got %d", len(rec.artifacts))
	}
	if strings.Contains(string(rec.artifacts[0].data), secret) {
		t.Fatalf("recorded artifact still contains the raw secret: %q", rec.artifacts[0].data)
	}
	if !strings.Contains(string(rec.artifacts[0].data), journal.Redacted) {
		t.Fatalf("recorded artifact missing redaction placeholder: %q", rec.artifacts[0].data)
	}
}

// TestExecutorMaterializesContextArtifactIntoWorkspace is the regression
// test for #121: a declared ContextPointer's in-journal artifact must be
// resolved into the stage's workspace before the harness session runs, so
// the harness's own tools can actually read it — not just see an opaque
// pointer name.
func TestExecutorMaterializesContextArtifactIntoWorkspace(t *testing.T) {
	journalRoot := t.TempDir()
	content := []byte("upstream diff content\n")
	ptr, err := apiv1.WriteArtifact(journalRoot, "artifacts/impl/diff.patch", content, "text/x-patch")
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}

	var gotBytes []byte
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			gotPath, ok := req.ContextPaths["implement.artifact[0]"]
			if !ok {
				return errors.New("context path not resolved")
			}
			data, rerr := os.ReadFile(filepath.Join(req.Workspace, gotPath))
			if rerr != nil {
				return rerr
			}
			gotBytes = data
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	rec := &fakeRecorder{dir: journalRoot}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(t.TempDir())
	env.ContextPointers = []apiv1.ContextPointer{{Name: "implement.artifact[0]", Artifact: &ptr}}
	result, err := exec.Invoke(context.Background(), env)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("Status = %q, want success", result.Status)
	}
	if string(gotBytes) != string(content) {
		t.Fatalf("materialized context bytes = %q, want %q", gotBytes, content)
	}
}

// TestExecutorFailsHardWhenContextArtifactCannotBeResolved covers the
// integrity-fault path: an upstream artifact this stage was promised is
// missing from the journal. That is not a normal stage failure the way a
// missing declared output file is — it propagates as a hard executor error.
func TestExecutorFailsHardWhenContextArtifactCannotBeResolved(t *testing.T) {
	journalRoot := t.TempDir() // nothing written — the pointer resolves to nothing
	badPtr := apiv1.ArtifactPointer{Path: "artifacts/missing", Digest: apiv1.Digest([]byte("x"))}

	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	rec := &fakeRecorder{dir: journalRoot}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(t.TempDir())
	env.ContextPointers = []apiv1.ContextPointer{{Name: "implement.artifact[0]", Artifact: &badPtr}}
	if _, err := exec.Invoke(context.Background(), env); err == nil {
		t.Fatal("expected a hard error when an upstream context artifact cannot be resolved")
	}
}

// TestExecutorResolvesCrossRunContextPointerWhenCapabilityDeclared is
// issue #103/T3's core positive case: a ContextPointer naming ANOTHER run
// (RunID set) resolves read-only and digest-verified into the stage
// workspace when journal:read is declared, exactly like a same-run pointer
// (#121) — and the resolved path lands in the rendered prompt (evidence
// links, per the design doc's test plan), not just the workspace file.
func TestExecutorResolvesCrossRunContextPointerWhenCapabilityDeclared(t *testing.T) {
	runsDir := t.TempDir()
	otherRunDir := filepath.Join(runsDir, "other-run-abc123")
	if err := os.MkdirAll(otherRunDir, 0o755); err != nil {
		t.Fatalf("mkdir other run dir: %v", err)
	}
	content := []byte("flagged run transcript excerpt\n")
	ptr, err := apiv1.WriteArtifact(otherRunDir, "artifacts/diagnose/evidence.txt", content, "text/plain")
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}

	var gotPath string
	var gotBytes []byte
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			path, ok := req.ContextPaths["flagged-run.evidence"]
			if !ok {
				return errors.New("context path not resolved")
			}
			gotPath = path
			data, rerr := os.ReadFile(filepath.Join(req.Workspace, path))
			if rerr != nil {
				return rerr
			}
			gotBytes = data
			if !strings.Contains(renderPrompt(req), path) {
				return fmt.Errorf("rendered prompt does not reference resolved evidence path %q", path)
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	rec := &fakeRecorder{dir: t.TempDir(), runsDir: runsDir}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(t.TempDir(), string(capability.JournalRead))
	env.ContextPointers = []apiv1.ContextPointer{
		{Name: "flagged-run.evidence", Artifact: &ptr, RunID: "other-run-abc123"},
	}
	result, err := exec.Invoke(context.Background(), env)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("Status = %q, want success", result.Status)
	}
	if string(gotBytes) != string(content) {
		t.Fatalf("materialized cross-run bytes = %q, want %q", gotBytes, content)
	}
	if gotPath == "" {
		t.Fatal("expected a non-empty resolved path")
	}
}

// TestExecutorRefusesCrossRunContextPointerWithoutCapability is T3's
// fail-closed test-plan item: a ContextPointer naming another run is
// refused when the stage did NOT declare journal:read, even though the
// pointer itself (path + digest) is perfectly valid — the capability check
// runs before resolution is even attempted.
func TestExecutorRefusesCrossRunContextPointerWithoutCapability(t *testing.T) {
	runsDir := t.TempDir()
	otherRunDir := filepath.Join(runsDir, "other-run-abc123")
	if err := os.MkdirAll(otherRunDir, 0o755); err != nil {
		t.Fatalf("mkdir other run dir: %v", err)
	}
	ptr, err := apiv1.WriteArtifact(otherRunDir, "artifacts/diagnose/evidence.txt", []byte("secret evidence"), "text/plain")
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}

	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	rec := &fakeRecorder{dir: t.TempDir(), runsDir: runsDir}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	// No journal:read in the declared capabilities.
	env := testEnvelope(t.TempDir())
	env.ContextPointers = []apiv1.ContextPointer{
		{Name: "flagged-run.evidence", Artifact: &ptr, RunID: "other-run-abc123"},
	}
	if _, err := exec.Invoke(context.Background(), env); !errors.Is(err, ErrJournalReadRequired) {
		t.Fatalf("Invoke error = %v, want ErrJournalReadRequired", err)
	}
}

// TestExecutorRefusesCrossRunRunIDPathEscape is T3's path-escape test-plan
// item: a RunID that is not a single safe path segment (a traversal or
// absolute-path attempt smuggled in via, e.g., a tampered candidate-findings
// artifact — SEC-047's threat model) is refused before it is ever joined
// onto RunsDir, even with journal:read declared.
func TestExecutorRefusesCrossRunRunIDPathEscape(t *testing.T) {
	runsDir := t.TempDir()
	// A syntactically valid pointer — the escape is entirely in RunID.
	ptr := apiv1.ArtifactPointer{Path: "artifacts/x", Digest: apiv1.Digest([]byte("x"))}

	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	rec := &fakeRecorder{dir: t.TempDir(), runsDir: runsDir}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	for _, runID := range []string{"../../etc", "/etc/passwd", "..", "a/b"} {
		env := testEnvelope(t.TempDir(), string(capability.JournalRead))
		env.ContextPointers = []apiv1.ContextPointer{
			{Name: "flagged-run.evidence", Artifact: &ptr, RunID: runID},
		}
		if _, err := exec.Invoke(context.Background(), env); !errors.Is(err, ErrInvalidRunID) {
			t.Fatalf("RunID %q: Invoke error = %v, want ErrInvalidRunID", runID, err)
		}
	}
}

// TestExecutorRefusesCrossRunDigestMismatch is T3's digest-verified
// test-plan item exercised cross-run: a pointer whose recorded digest no
// longer matches the OTHER run's on-disk bytes is refused exactly like the
// same-run case (TestExecutorFailsHardWhenContextArtifactCannotBeResolved)
// — the cross-run path reuses apiv1.ArtifactPointer.Resolve unchanged, so
// tampering detection is identical regardless of which run's journal root
// the pointer resolves against.
func TestExecutorRefusesCrossRunDigestMismatch(t *testing.T) {
	runsDir := t.TempDir()
	otherRunDir := filepath.Join(runsDir, "other-run-abc123")
	if err := os.MkdirAll(otherRunDir, 0o755); err != nil {
		t.Fatalf("mkdir other run dir: %v", err)
	}
	ptr, err := apiv1.WriteArtifact(otherRunDir, "artifacts/diagnose/evidence.txt", []byte("original"), "text/plain")
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	// Tamper with the bytes after the pointer's digest was computed.
	if err := os.WriteFile(filepath.Join(otherRunDir, "artifacts/diagnose/evidence.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper artifact: %v", err)
	}

	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	rec := &fakeRecorder{dir: t.TempDir(), runsDir: runsDir}
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(t.TempDir(), string(capability.JournalRead))
	env.ContextPointers = []apiv1.ContextPointer{
		{Name: "flagged-run.evidence", Artifact: &ptr, RunID: "other-run-abc123"},
	}
	if _, err := exec.Invoke(context.Background(), env); !errors.Is(err, apiv1.ErrDigestMismatch) {
		t.Fatalf("Invoke error = %v, want ErrDigestMismatch", err)
	}
}

// noopRegistrar discards secrets — used by tests that don't exercise redaction.
type noopRegistrar struct{}

func (noopRegistrar) Register(secret []byte) {}

// agentModelInjector grants exactly agent:model from a real env-var token ref —
// the reviewer-gate credential shape #294 introduces.
func agentModelInjector(t *testing.T, token string) *credentials.Injector {
	t.Helper()
	t.Setenv("MODEL_TOKEN_ENV", token)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "model-token", Env: "MODEL_TOKEN_ENV"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: "agent:model", Ref: "model-token"}}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	return injector
}

// TestExecutorReviewMaterializesAgentModel is #294's Review-path proof: the
// reviewer runs through Executor.Review (ModeReview), and its declared
// agent:model capability is materialized and handed to the adapter — so the
// reviewer subprocess can authenticate the Copilot model exactly like a task
// stage. Complements the runner test (the gate ENVELOPE now carries the cap)
// and #288's adapter test (the cap becomes COPILOT_GITHUB_TOKEN in the env).
func TestExecutorReviewMaterializesAgentModel(t *testing.T) {
	var gotToken string
	var gotMode Mode
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			gotMode = req.Mode
			var err error
			gotToken, err = req.Credentials.Token(ctx, "agent:model")
			if err != nil {
				return err
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "ok"})
		},
	}
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, agentModelInjector(t, "copilot-model-token"), rec, rec, rec, journal.NewPatternScrubber(), "review it")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	verdict, err := exec.Review(context.Background(), testEnvelope(t.TempDir(), "agent:model"))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Decision != apiv1.VerdictPass {
		t.Fatalf("decision = %q, want pass", verdict.Decision)
	}
	if gotMode != ModeReview {
		t.Fatalf("adapter mode = %q, want review", gotMode)
	}
	if gotToken != "copilot-model-token" {
		t.Fatalf("agent:model token = %q, want copilot-model-token", gotToken)
	}
}
