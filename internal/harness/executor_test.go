package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
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

// fakeRecorder is an in-memory SpanRecorder double.
type fakeRecorder struct {
	spans []recordedSpan
	err   error
}

type recordedSpan struct {
	stage, name string
	data        []byte
}

func (f *fakeRecorder) RecordSpan(stage, name string, data []byte) (journal.Ref, error) {
	if f.err != nil {
		return journal.Ref{}, f.err
	}
	f.spans = append(f.spans, recordedSpan{stage: stage, name: name, data: append([]byte(nil), data...)})
	return journal.Ref{Digest: journal.Digest(data)}, nil
}

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
	exec, err := NewExecutor(adapter, injector, rec, journal.NewPatternScrubber(), "be a good coder")
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
	if string(rec.spans[0].data) != "implementing... done" {
		t.Fatalf("span data = %q", rec.spans[0].data)
	}
}

func TestExecutorInvokeFailsClosedOnMissingCompletion(t *testing.T) {
	adapter := &FakeAdapter{} // Act is nil: writes nothing
	injector := testInjector(t, "", "", noopRegistrar{})
	exec, err := NewExecutor(adapter, injector, &fakeRecorder{}, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, &fakeRecorder{}, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, &fakeRecorder{}, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, &fakeRecorder{}, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, &fakeRecorder{}, journal.NewPatternScrubber(), "")
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
	adapter := &FakeAdapter{
		Transcript: []byte("logging in with token=" + secret + " now"),
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	injector := testInjector(t, "REPO_TOKEN_ENV", secret, reg)
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, scrub, "")
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

// noopRegistrar discards secrets — used by tests that don't exercise redaction.
type noopRegistrar struct{}

func (noopRegistrar) Register(secret []byte) {}
