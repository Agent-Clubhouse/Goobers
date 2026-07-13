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

// fakeRecorder is an in-memory SpanRecorder + ArtifactRecorder double — mirrors
// production, where the same *journal.Run satisfies both structurally.
type fakeRecorder struct {
	spans     []recordedSpan
	artifacts []recordedArtifact
	err       error
}

type recordedSpan struct {
	stage, name string
	data        []byte
}

type recordedArtifact struct {
	name string
	data []byte
}

func (f *fakeRecorder) RecordSpan(stage, name string, data []byte) (journal.Ref, error) {
	if f.err != nil {
		return journal.Ref{}, f.err
	}
	f.spans = append(f.spans, recordedSpan{stage: stage, name: name, data: append([]byte(nil), data...)})
	return journal.Ref{Digest: journal.Digest(data)}, nil
}

func (f *fakeRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	if f.err != nil {
		return journal.Ref{}, f.err
	}
	f.artifacts = append(f.artifacts, recordedArtifact{name: name, data: append([]byte(nil), data...)})
	return journal.Ref{Path: "artifacts/fake/" + name, Digest: journal.Digest(data), Size: int64(len(data))}, nil
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
	exec, err := NewExecutor(adapter, injector, rec, rec, journal.NewPatternScrubber(), "be a good coder")
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
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, rec, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, rec, rec, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, rec, rec, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, rec, rec, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, rec, rec, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, rec, rec, scrub, "")
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
	exec, err := NewExecutor(adapter, injector, rec, rec, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, rec, rec, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, rec, rec, journal.NewPatternScrubber(), "")
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
	exec, err := NewExecutor(adapter, injector, rec, rec, scrub, "")
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

// noopRegistrar discards secrets — used by tests that don't exercise redaction.
type noopRegistrar struct{}

func (noopRegistrar) Register(secret []byte) {}
