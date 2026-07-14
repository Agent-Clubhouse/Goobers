package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

// fakeRecorder is an in-memory ArtifactRecorder for tests: no real journal
// directory needed.
type fakeRecorder struct {
	recorded map[string][]byte
}

func newFakeRecorder() *fakeRecorder { return &fakeRecorder{recorded: map[string][]byte{}} }

func (f *fakeRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	cp := make([]byte, len(data))
	copy(cp, data)
	f.recorded[name] = cp
	return journal.Ref{Path: name, Digest: journal.Digest(cp), Size: int64(len(cp))}, nil
}

// noopRegistrar satisfies credentials.SecretRegistrar for tests that don't
// care about scrub-registration (ShellExecutor.Run builds and uses its own
// scrubber independently of the Injector's registrar).
type noopRegistrar struct{}

func (noopRegistrar) Register([]byte) {}

func newTestInjector(t *testing.T, capability, envVar, value string) *credentials.Injector {
	t.Helper()
	t.Setenv(envVar, value)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "ref", Env: envVar}})
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: capability, Ref: "ref"}}, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	return injector
}

func newTestExecutor(t *testing.T, injector *credentials.Injector) (*ShellExecutor, *fakeRecorder) {
	t.Helper()
	if injector == nil {
		var err error
		injector, err = credentials.NewInjector(&credentials.Resolver{}, nil, noopRegistrar{})
		if err != nil {
			t.Fatal(err)
		}
	}
	rec := newFakeRecorder()
	exec, err := NewShellExecutor(injector, rec)
	if err != nil {
		t.Fatal(err)
	}
	return exec, rec
}

func baseEnvelope(t *testing.T) apiv1.InvocationEnvelope {
	t.Helper()
	return apiv1.InvocationEnvelope{TaskID: "task-1", Workspace: t.TempDir()}
}

func TestShellExecutor_RunSuccess(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	env := baseEnvelope(t)

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "echo hello"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (result: %+v)", result.Status, result)
	}
	if len(result.Artifacts) != 2 {
		t.Fatalf("expected stdout+stderr artifacts, got %d", len(result.Artifacts))
	}
	if got := string(rec.recorded["task-1/stdout.log"]); !strings.Contains(got, "hello") {
		t.Fatalf("stdout artifact = %q, want it to contain %q", got, "hello")
	}
}

func TestShellExecutor_NonZeroExit(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "nonzero_exit" || result.Error.Retryable {
		t.Fatalf("error = %+v, want nonzero_exit, non-retryable", result.Error)
	}
	if result.Metrics["exitCode"] != 3 {
		t.Fatalf("exitCode metric = %v, want 3", result.Metrics["exitCode"])
	}
}

func TestShellExecutor_TimeoutKillsProcessGroup(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "100ms"}

	start := time.Now()
	// A background child under a parent that waits on it — proves the whole
	// group dies, not just the directly-exec'd shell.
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", "sleep 30 & wait"},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run took %s, want well under the 30s sleep — process group was not killed", elapsed)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "timeout" || !result.Error.Retryable {
		t.Fatalf("error = %+v, want timeout, retryable", result.Error)
	}
}

func TestShellExecutor_CanarySecretNeverInCapturedOutput(t *testing.T) {
	const canary = "s3cr3t-canary-token-value"
	// Negative control: this canary must NOT be a shape the pattern-net catches
	// on its own, or a passing test below wouldn't prove the registry scrubber
	// (which redacts by exact registered value) is what's doing the work.
	if got := journal.NewPatternScrubber().Scrub([]byte(canary)); string(got) != canary {
		t.Fatalf("test setup: canary %q is pattern-net-catchable (scrubbed to %q) — this test would pass even if registry scrubbing were broken; use an opaque value with no recognizable credential shape", canary, got)
	}

	injector := newTestInjector(t, "test:cap", "GOOBERS_TEST_CANARY", canary)
	exec, rec := newTestExecutor(t, injector)
	env := baseEnvelope(t)
	env.Capabilities = []string{"test:cap"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", "echo $GOOBERS_CRED_TEST_CAP"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	stdout := string(rec.recorded["task-1/stdout.log"])
	if strings.Contains(stdout, canary) {
		t.Fatalf("captured stdout contains the raw canary secret: %q", stdout)
	}
	if !strings.Contains(stdout, journal.Redacted) {
		t.Fatalf("captured stdout = %q, want the redaction marker present", stdout)
	}
}

func TestShellExecutor_CapabilityInjectedAsEnvVar(t *testing.T) {
	injector := newTestInjector(t, "test:cap", "GOOBERS_TEST_TOKEN", "token-value-123")
	exec, rec := newTestExecutor(t, injector)
	env := baseEnvelope(t)
	env.Capabilities = []string{"test:cap"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `test -n "$GOOBERS_CRED_TEST_CAP" && echo present`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (the credential env var should have been set)", result.Status)
	}
	if !strings.Contains(string(rec.recorded["task-1/stdout.log"]), "present") {
		t.Fatalf("expected stdout to show the credential env var was non-empty")
	}
}

func TestShellExecutor_UndeclaredCapabilityNotInjected(t *testing.T) {
	// Injector is configured for "test:cap", but the stage does not declare
	// it — fail-closed: no credential should be materialized or injected.
	injector := newTestInjector(t, "test:cap", "GOOBERS_TEST_UNDECLARED", "should-not-appear")
	exec, rec := newTestExecutor(t, injector)
	env := baseEnvelope(t) // no Capabilities declared

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `test -z "$GOOBERS_CRED_TEST_CAP" && echo absent`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (env var should be absent)", result.Status)
	}
	if !strings.Contains(string(rec.recorded["task-1/stdout.log"]), "absent") {
		t.Fatalf("expected the undeclared capability's env var to be unset")
	}
}

func TestShellExecutor_ResultFileLiftedToArtifact(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "out.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo '{"ok":true}' > out.json`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if len(result.Artifacts) != 3 {
		t.Fatalf("expected stdout+stderr+result artifacts, got %d", len(result.Artifacts))
	}
	if !strings.Contains(string(rec.recorded["task-1/result"]), `"ok":true`) {
		t.Fatalf("result artifact missing expected content: %v", rec.recorded["task-1/result"])
	}
}

func TestShellExecutor_MissingDeclaredResultFileIsFailure(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "never-written.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "missing_result_file" {
		t.Fatalf("error = %+v, want missing_result_file", result.Error)
	}
}

// TestShellExecutor_ResultFilePathTraversalIsRejected is the regression test
// for #120: a declared resultFile that lexically escapes the workspace (via
// "..") must fail the stage closed, never lift the escaped file's content
// into a recorded artifact.
func TestShellExecutor_ResultFilePathTraversalIsRejected(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := []byte(`{"leaked":true}`)
	if err := os.WriteFile(filepath.Join(parent, "secret.json"), secret, 0o644); err != nil {
		t.Fatal(err)
	}

	exec, rec := newTestExecutor(t, nil)
	env := apiv1.InvocationEnvelope{TaskID: "task-1", Workspace: workspace}
	env.Inputs = map[string]interface{}{InputResultFile: "../secret.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "result_file_path_escape" {
		t.Fatalf("error = %+v, want result_file_path_escape", result.Error)
	}
	for name, data := range rec.recorded {
		if strings.Contains(string(data), "leaked") {
			t.Fatalf("escaped file content leaked into recorded artifact %q: %s", name, data)
		}
	}
}

// TestShellExecutor_ResultFileSymlinkEscapeIsRejected is the symlink half of
// #120: a declared resultFile name that is lexically contained but is itself
// a symlink to a file outside the workspace must also fail the stage closed.
func TestShellExecutor_ResultFileSymlinkEscapeIsRejected(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	secret := []byte(`{"leaked":true}`)
	if err := os.WriteFile(filepath.Join(outside, "secret.json"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.json"), filepath.Join(workspace, "out.json")); err != nil {
		t.Fatal(err)
	}

	exec, rec := newTestExecutor(t, nil)
	env := apiv1.InvocationEnvelope{TaskID: "task-1", Workspace: workspace}
	env.Inputs = map[string]interface{}{InputResultFile: "out.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "result_file_path_escape" {
		t.Fatalf("error = %+v, want result_file_path_escape", result.Error)
	}
	for name, data := range rec.recorded {
		if strings.Contains(string(data), "leaked") {
			t.Fatalf("symlinked outside file content leaked into recorded artifact %q: %s", name, data)
		}
	}
}

func TestShellExecutor_OutputTruncation(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	exec.DefaultMaxOutputBytes = 8
	env := baseEnvelope(t)

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", "echo 0123456789abcdef"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs["stdoutTruncated"] != true {
		t.Fatalf("outputs = %+v, want stdoutTruncated=true", result.Outputs)
	}
	if got := len(rec.recorded["task-1/stdout.log"]); got != 8 {
		t.Fatalf("captured stdout length = %d, want capped at 8", got)
	}
}

func TestShellExecutor_RunsInDeclaredWorkspace(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	if err := os.WriteFile(filepath.Join(env.Workspace, "marker.txt"), []byte("present\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"cat", "marker.txt"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(rec.recorded["task-1/stdout.log"]), "present") {
		t.Fatalf("command did not appear to run in env.Workspace")
	}
}

func TestShellExecutor_EmptyCommandIsConfigError(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	_, err := exec.Run(context.Background(), baseEnvelope(t), apiv1.DeterministicRun{})
	if err == nil {
		t.Fatal("expected an error for an empty Command")
	}
}

func TestNewShellExecutor_RequiresInjectorAndJournal(t *testing.T) {
	if _, err := NewShellExecutor(nil, newFakeRecorder()); err == nil {
		t.Fatal("expected error for nil injector")
	}
	injector, err := credentials.NewInjector(&credentials.Resolver{}, nil, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewShellExecutor(injector, nil); err == nil {
		t.Fatal("expected error for nil journal recorder")
	}
}
