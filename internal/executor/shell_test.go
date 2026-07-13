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

// noopRegistrar satisfies credentials.SecretRegistrar for building test
// Injectors — this package's own scrubbing is independent of it.
type noopRegistrar struct{}

func (noopRegistrar) Register([]byte) {}

// newTestTokenSource materializes a real *credentials.Set for capability,
// backed by envVar=value, so tests exercise the actual fail-closed
// credentials.Set.Token contract rather than a hand-rolled fake.
func newTestTokenSource(t *testing.T, capability, envVar, value string) TokenSource {
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
	set, err := injector.Materialize(context.Background(), []string{capability})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func artifact(produced []ProducedArtifact, name string) []byte {
	for _, p := range produced {
		if p.Name == name {
			return p.Data
		}
	}
	return nil
}

func TestShellExecutor_RunSuccess(t *testing.T) {
	exec := NewShellExecutor()
	result, produced, err := exec.Run(context.Background(), t.TempDir(), nil, nil, ShellConfig{Command: []string{"sh", "-c", "echo hello"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (result: %+v)", result.Status, result)
	}
	if len(produced) != 2 {
		t.Fatalf("expected stdout+stderr artifacts, got %d", len(produced))
	}
	if got := string(artifact(produced, "stdout.log")); !strings.Contains(got, "hello") {
		t.Fatalf("stdout artifact = %q, want it to contain %q", got, "hello")
	}
	if len(result.Artifacts) != 0 {
		t.Fatalf("expected ResultEnvelope.Artifacts to stay empty (caller commits Produced), got %v", result.Artifacts)
	}
}

func TestShellExecutor_NonZeroExit(t *testing.T) {
	exec := NewShellExecutor()
	result, _, err := exec.Run(context.Background(), t.TempDir(), nil, nil, ShellConfig{Command: []string{"sh", "-c", "exit 3"}})
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
	exec := NewShellExecutor()
	start := time.Now()
	// A background child under a parent that waits on it — proves the whole
	// group dies, not just the directly-exec'd shell.
	result, _, err := exec.Run(context.Background(), t.TempDir(), nil, nil, ShellConfig{
		Command: []string{"sh", "-c", "sleep 30 & wait"},
		Timeout: 100 * time.Millisecond,
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
	tokens := newTestTokenSource(t, "test:cap", "GOOBERS_TEST_CANARY", canary)
	exec := NewShellExecutor()

	result, produced, err := exec.Run(context.Background(), t.TempDir(), []string{"test:cap"}, tokens, ShellConfig{
		Command: []string{"sh", "-c", "echo $GOOBERS_CRED_TEST_CAP"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	stdout := string(artifact(produced, "stdout.log"))
	if strings.Contains(stdout, canary) {
		t.Fatalf("captured stdout contains the raw canary secret: %q", stdout)
	}
	if !strings.Contains(stdout, journal.Redacted) {
		t.Fatalf("captured stdout = %q, want the redaction marker present", stdout)
	}
}

func TestShellExecutor_CapabilityInjectedAsEnvVar(t *testing.T) {
	tokens := newTestTokenSource(t, "test:cap", "GOOBERS_TEST_TOKEN", "token-value-123")
	exec := NewShellExecutor()

	result, produced, err := exec.Run(context.Background(), t.TempDir(), []string{"test:cap"}, tokens, ShellConfig{
		Command: []string{"sh", "-c", `test -n "$GOOBERS_CRED_TEST_CAP" && echo present`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (the credential env var should have been set)", result.Status)
	}
	if !strings.Contains(string(artifact(produced, "stdout.log")), "present") {
		t.Fatalf("expected stdout to show the credential env var was non-empty")
	}
}

func TestShellExecutor_UndeclaredCapabilityNotInjected(t *testing.T) {
	// The token source COULD resolve "test:cap", but Run is called without it
	// in the capabilities list — fail-closed: no credential should be
	// injected for a capability this specific invocation doesn't declare.
	tokens := newTestTokenSource(t, "test:cap", "GOOBERS_TEST_UNDECLARED", "should-not-appear")
	exec := NewShellExecutor()

	result, produced, err := exec.Run(context.Background(), t.TempDir(), nil, tokens, ShellConfig{
		Command: []string{"sh", "-c", `test -z "$GOOBERS_CRED_TEST_CAP" && echo absent`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (env var should be absent)", result.Status)
	}
	if !strings.Contains(string(artifact(produced, "stdout.log")), "absent") {
		t.Fatalf("expected the undeclared capability's env var to be unset")
	}
}

func TestShellExecutor_ResultFileLiftedToArtifact(t *testing.T) {
	exec := NewShellExecutor()
	result, produced, err := exec.Run(context.Background(), t.TempDir(), nil, nil, ShellConfig{
		Command:    []string{"sh", "-c", `echo '{"ok":true}' > out.json`},
		ResultFile: "out.json",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if len(produced) != 3 {
		t.Fatalf("expected stdout+stderr+result artifacts, got %d", len(produced))
	}
	if !strings.Contains(string(artifact(produced, "result")), `"ok":true`) {
		t.Fatalf("result artifact missing expected content: %v", artifact(produced, "result"))
	}
}

func TestShellExecutor_MissingDeclaredResultFileIsFailure(t *testing.T) {
	exec := NewShellExecutor()
	result, _, err := exec.Run(context.Background(), t.TempDir(), nil, nil, ShellConfig{
		Command:    []string{"sh", "-c", "exit 0"},
		ResultFile: "never-written.json",
	})
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

func TestShellExecutor_OutputTruncation(t *testing.T) {
	exec := NewShellExecutor()
	exec.DefaultMaxOutputBytes = 8

	result, produced, err := exec.Run(context.Background(), t.TempDir(), nil, nil, ShellConfig{
		Command: []string{"sh", "-c", "echo 0123456789abcdef"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs["stdoutTruncated"] != true {
		t.Fatalf("outputs = %+v, want stdoutTruncated=true", result.Outputs)
	}
	if got := len(artifact(produced, "stdout.log")); got != 8 {
		t.Fatalf("captured stdout length = %d, want capped at 8", got)
	}
}

func TestShellExecutor_RunsInDeclaredWorkspace(t *testing.T) {
	exec := NewShellExecutor()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "marker.txt"), []byte("present\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, produced, err := exec.Run(context.Background(), workspace, nil, nil, ShellConfig{Command: []string{"cat", "marker.txt"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(artifact(produced, "stdout.log")), "present") {
		t.Fatalf("command did not appear to run in the declared workspace")
	}
}

func TestShellExecutor_EmptyCommandIsConfigError(t *testing.T) {
	exec := NewShellExecutor()
	_, _, err := exec.Run(context.Background(), t.TempDir(), nil, nil, ShellConfig{})
	if err == nil {
		t.Fatal("expected an error for an empty Command")
	}
}

func TestConfigFromEnvelope(t *testing.T) {
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{
		InputTimeout:        "5m",
		InputResultFile:     "out.json",
		InputMaxOutputBytes: "2048",
	}}
	cfg, err := ConfigFromEnvelope(env, apiv1.DeterministicRun{Command: []string{"make", "ci"}})
	if err != nil {
		t.Fatalf("ConfigFromEnvelope: %v", err)
	}
	if cfg.Timeout != 5*time.Minute || cfg.ResultFile != "out.json" || cfg.MaxOutputBytes != 2048 {
		t.Fatalf("cfg = %+v, unexpected", cfg)
	}
	if len(cfg.Command) != 2 || cfg.Command[0] != "make" {
		t.Fatalf("cfg.Command = %v, want run.Command carried through", cfg.Command)
	}
}

func TestConfigFromEnvelope_InvalidTimeoutIsError(t *testing.T) {
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputTimeout: "not-a-duration"}}
	if _, err := ConfigFromEnvelope(env, apiv1.DeterministicRun{Command: []string{"true"}}); err == nil {
		t.Fatal("expected an error for a malformed timeout input")
	}
}
