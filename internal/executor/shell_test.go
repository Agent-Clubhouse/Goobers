package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/invoke"
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

func TestShellExecutor_ClassifiesProviderBuiltinFailures(t *testing.T) {
	cases := []struct {
		name               string
		message            string
		wantInfrastructure bool
	}{
		{"server error", "error: list work items: GET failed: status 503: unavailable", true},
		{"transport timeout", "error: list work items: net/http: TLS handshake timeout", true},
		{"authentication", "error: list work items: GET failed: status 401: bad credentials", false},
		{"authorization", "error: list work items: GET failed: status 403: forbidden", false},
		{"deterministic request", "error: list work items: GET failed: status 422: invalid query", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "goobers")
			body := "#!/bin/sh\n" +
				"echo \"" + tc.message + "\" >&2\n" +
				"exit 1\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}

			exec, rec := newTestExecutor(t, nil)
			exec.SelfBin = script
			env := baseEnvelope(t)
			env.Inputs = map[string]interface{}{InputResultFile: "provider-result.json"}
			result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
				Command: []string{"goobers", "backlog-query", "--claim"},
			})

			if got := invoke.IsInfrastructureFailure(err); got != tc.wantInfrastructure {
				t.Fatalf("infrastructure failure = %v, want %v (result=%+v, err=%v)", got, tc.wantInfrastructure, result, err)
			}
			if tc.wantInfrastructure {
				if !strings.Contains(err.Error(), tc.message) {
					t.Fatalf("error %q does not preserve provider cause %q", err, tc.message)
				}
			} else if err != nil {
				t.Fatalf("Run returned dispatch error for non-retryable provider failure: %v", err)
			} else if result.Error == nil || result.Error.Code != "provider_error" || result.Error.Retryable || result.Error.Message != tc.message {
				t.Fatalf("result error = %+v, want non-retryable provider_error preserving %q", result.Error, tc.message)
			}
			if got := string(rec.recorded["task-1/stderr.log"]); !strings.Contains(got, tc.message) {
				t.Fatalf("stderr artifact = %q, want provider cause %q", got, tc.message)
			}
		})
	}
}

// TestShellExecutor_ProviderBuiltinPrefersStructuredErrorCodeOverStderr is
// the #613/#711/#712 precedence ruling's core contract: when a
// provider-builtin stage exits nonzero AND has already self-reported a
// structured OutputErrorCode via its declared result file
// (failProviderStage, #614), that code is authoritative — stderr text is
// never consulted, even when it would classify differently. Retryable ->
// InfrastructureFailure (feeding the runner's retry budget); non-retryable
// -> a ResultFailure carrying that EXACT code, not the generic
// "provider_error" the stderr fallback would have produced.
func TestShellExecutor_ProviderBuiltinPrefersStructuredErrorCodeOverStderr(t *testing.T) {
	cases := []struct {
		name               string
		resultJSON         string
		wantInfrastructure bool
		wantCode           string
		wantMessage        string
	}{
		{
			name:               "retryable code drives infrastructure failure",
			resultJSON:         `{"errorCode":"github_rate_limited","errorMessage":"rate limited, resets soon","errorRetryable":true,"rateLimitReset":"2026-07-17T04:00:00Z"}`,
			wantInfrastructure: true,
			wantMessage:        "rate limited, resets soon",
		},
		{
			name:               "non-retryable code stays a plain ResultFailure with its own code",
			resultJSON:         `{"errorCode":"github_auth_failed","errorMessage":"bad credentials","errorRetryable":false}`,
			wantInfrastructure: false,
			wantCode:           "github_auth_failed",
			wantMessage:        "bad credentials",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "goobers")
			// The stderr text says the OPPOSITE of the structured code on
			// purpose (a 503, always transient by stderr-only rules) — if
			// this test passes, the structured code won this decision, not
			// stderr, proving real precedence rather than a coincidental
			// agreement between the two signals.
			body := "#!/bin/sh\n" +
				"cat > provider-result.json <<'EOF'\n" + tc.resultJSON + "\nEOF\n" +
				"echo \"error: list work items: GET failed: status 503: unavailable\" >&2\n" +
				"exit 1\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}

			exec, rec := newTestExecutor(t, nil)
			exec.SelfBin = script
			env := baseEnvelope(t)
			env.Inputs = map[string]interface{}{InputResultFile: "provider-result.json"}
			result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
				Command: []string{"goobers", "backlog-query", "--claim"},
			})

			if got := invoke.IsInfrastructureFailure(err); got != tc.wantInfrastructure {
				t.Fatalf("infrastructure failure = %v, want %v (result=%+v, err=%v)", got, tc.wantInfrastructure, result, err)
			}
			if tc.wantInfrastructure {
				if !strings.Contains(err.Error(), tc.wantMessage) {
					t.Fatalf("error %q does not preserve structured message %q", err, tc.wantMessage)
				}
				if strings.Contains(err.Error(), "status 503") {
					t.Fatalf("error %q leaked the stderr-derived message — structured code did not win", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run returned dispatch error for a non-retryable structured code: %v", err)
			}
			if result.Error == nil || result.Error.Code != tc.wantCode || result.Error.Retryable || result.Error.Message != tc.wantMessage {
				t.Fatalf("result error = %+v, want code=%s non-retryable message=%q", result.Error, tc.wantCode, tc.wantMessage)
			}
			if result.Error.Code == "provider_error" {
				t.Fatalf("result error code = %q, want the structured code preserved, not the generic stderr fallback", result.Error.Code)
			}
			if got := string(rec.recorded["task-1/result"]); !strings.Contains(got, tc.wantCode) {
				t.Fatalf("recorded result artifact = %q, want it to contain the structured code %q", got, tc.wantCode)
			}
		})
	}
}

// TestShellExecutor_ProviderBuiltinFallsBackToStderrWithoutStructuredResult
// proves the residual case still works exactly as before this precedence
// check existed: a provider-builtin stage that declares a result file but
// never writes one (died before it could call failProviderStage at all —
// bad flags, signal kill, panic) still gets classified from stderr text,
// not silently swallowed or misrouted.
func TestShellExecutor_ProviderBuiltinFallsBackToStderrWithoutStructuredResult(t *testing.T) {
	script := filepath.Join(t.TempDir(), "goobers")
	body := "#!/bin/sh\n" +
		"echo \"error: list work items: GET failed: status 503: unavailable\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	exec, _ := newTestExecutor(t, nil)
	exec.SelfBin = script
	env := baseEnvelope(t)
	// Declares a result file the script never writes — the crashed-before-
	// self-reporting case.
	env.Inputs = map[string]interface{}{InputResultFile: "never-written.json"}
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"goobers", "backlog-query", "--claim"},
	})
	if !invoke.IsInfrastructureFailure(err) {
		t.Fatalf("Run result=%+v err=%v, want infrastructure failure from the stderr fallback", result, err)
	}
	if !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("error %q does not preserve the stderr-derived cause", err)
	}
}

func TestShellExecutor_ProviderClassificationUsesTerminalError(t *testing.T) {
	script := filepath.Join(t.TempDir(), "goobers")
	body := "#!/bin/sh\n" +
		"echo \"warning: prior request failed: status 503: unavailable\" >&2\n" +
		"echo \"error: invalid local configuration\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	exec, _ := newTestExecutor(t, nil)
	exec.SelfBin = script
	env := baseEnvelope(t)
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"goobers", "backlog-query", "--claim"},
	})
	if err != nil {
		t.Fatalf("Run returned dispatch error from a stale warning: %v", err)
	}
	if result.Error == nil || result.Error.Code != "provider_error" || result.Error.Message != "error: invalid local configuration" {
		t.Fatalf("result error = %+v, want terminal deterministic cause", result.Error)
	}
}

func TestShellExecutor_RejectsUnknownNetworkMode(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)

	_, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", "exit 0"},
		Network: apiv1.NetworkMode("host"),
	})
	if err == nil || !strings.Contains(err.Error(), `unknown network mode "host"`) {
		t.Fatalf("Run error = %v, want unknown network mode", err)
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
	// The whole group is killed well before the 30s sleep. Allow for the
	// timeout SIGQUIT grace: sh's backgrounded job ignores SIGQUIT (POSIX
	// async-list disposition), so the group only dies once SIGKILL escalates
	// after timeoutDumpGrace — still an order of magnitude under 30s.
	if elapsed > 20*time.Second {
		t.Fatalf("Run took %s, want well under the 30s sleep — process group was not killed", elapsed)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "timeout" || !result.Error.Retryable {
		t.Fatalf("error = %+v, want timeout, retryable", result.Error)
	}
}

// TestShellExecutor_TimeoutSIGQUITsBeforeKillForDump verifies the
// timeout-diagnostics path: on timeout the executor SIGQUITs the process group
// FIRST (so a real Go stage would dump its goroutines to the captured output)
// and only escalates to SIGKILL if that doesn't exit within timeoutDumpGrace.
// A shell that traps SIGQUIT, prints a marker (standing in for a goroutine
// dump), and exits proves the marker reaches the captured stdout artifact —
// i.e. SIGQUIT was delivered and its output captured before any kill. A plain
// SIGKILL (the old behavior) would leave the marker absent.
func TestShellExecutor_TimeoutSIGQUITsBeforeKillForDump(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "100ms"}

	const marker = "__SIGQUIT_DUMP_MARKER__"
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		// Trap SIGQUIT -> print the marker and exit; otherwise block forever.
		Command: []string{"sh", "-c", `trap 'echo ` + marker + `; exit 0' QUIT; while :; do sleep 0.05; done`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("status=%v error=%+v, want a timeout failure", result.Status, result.Error)
	}
	if got := string(rec.recorded["task-1/stdout.log"]); !strings.Contains(got, marker) {
		t.Fatalf("stdout artifact = %q, want it to contain %q — SIGQUIT must precede SIGKILL so a Go stage can dump before dying", got, marker)
	}
}

func TestShellExecutor_ProviderBuiltinTimeoutIsInfrastructureFailure(t *testing.T) {
	script := filepath.Join(t.TempDir(), "goobers")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	exec, _ := newTestExecutor(t, nil)
	exec.SelfBin = script
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "100ms"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"goobers", "backlog-query", "--claim"},
	})
	if !invoke.IsInfrastructureFailure(err) {
		t.Fatalf("Run result=%+v err=%v, want infrastructure failure", result, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %q does not preserve deadline cause", err)
	}
}

// TestShellExecutor_TimeoutGivesUpOnEscapedDescendant is the regression test
// for #119's WaitDelay gap: a grandchild that escapes the process group
// (via job control's own new-pgid-per-background-job behavior, the portable
// stand-in for setsid) survives the group kill and keeps the stdout pipe
// open, so cmd.Wait() would never return on its own. Run must still return
// within groupKillWaitDelay of the timeout rather than hanging for the
// escaped process's full lifetime.
func TestShellExecutor_TimeoutGivesUpOnEscapedDescendant(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "100ms"}

	start := time.Now()
	// `set -m` gives the backgrounded sleep its own process group (the
	// portable equivalent of setsid) — it outlives bash's own near-immediate
	// exit and is never reached by the group kill (bash's group, not its
	// own). 30s comfortably exceeds groupKillWaitDelay (5s), so the test can
	// only pass via the give-up bound, not by the escaped process happening
	// to exit on its own first.
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"bash", "-c", "set -m; sleep 30 & sleep 0.1"},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The escaped descendant ignores/outruns both signals, so Run gives up
	// after the timeout SIGQUIT grace AND the SIGKILL give-up bound:
	// ~timeout + timeoutDumpGrace + groupKillWaitDelay. Still bounded — it does
	// not hang for the escaped process's full 30s lifetime.
	if elapsed > timeoutDumpGrace+groupKillWaitDelay+3*time.Second {
		t.Fatalf("Run took %s, want under ~%s (timeout + timeoutDumpGrace + groupKillWaitDelay) — the give-up bound did not engage", elapsed, 100*time.Millisecond+timeoutDumpGrace+groupKillWaitDelay)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("error = %+v, want timeout", result.Error)
	}
}

// TestShellExecutor_DistinguishesCancelFromTimeout is #122's low-priority
// defense-in-depth item: runCtx.Done() fires both when its own timeout
// elapses and when the caller's ctx is externally canceled, and the two must
// not be conflated — a canceled ctx should never come back as the "timeout"
// error code. internal/runner's dispatch always uses context.WithoutCancel
// today, so this path is otherwise unreachable in production; the test
// drives it directly by canceling ctx itself rather than through the runner.
func TestShellExecutor_DistinguishesCancelFromTimeout(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	shellExec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "10s"} // comfortably longer than the external cancel

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result, err := shellExec.Run(ctx, env, apiv1.DeterministicRun{Command: []string{"sleep", "5"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "canceled" || result.Error.Retryable {
		t.Fatalf("error = %+v, want canceled, non-retryable", result.Error)
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

// TestShellExecutor_DoesNotPassthroughAmbientDaemonEnv is the regression test
// for #122's missing-negative-control gap: internal/harness has had this
// check since #70, but internal/executor's identical SEC-045 allowlist never
// did. The subprocess must not inherit the daemon process's own environment
// wholesale, since that would leak any resolver-sourced credential env var
// (e.g. instance.yaml's token.env) into every stage regardless of whether it
// declared the corresponding capability.
func TestShellExecutor_DoesNotPassthroughAmbientDaemonEnv(t *testing.T) {
	const ambientSecretVar = "GOOBERS_AMBIENT_DAEMON_SECRET"
	t.Setenv(ambientSecretVar, "ambient-daemon-secret-never-declared")

	exec, rec := newTestExecutor(t, nil)
	env := baseEnvelope(t) // no capabilities declared at all

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `test -z "$` + ambientSecretVar + `" && echo absent; echo "PATH=$PATH" | head -c 5`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	stdout := string(rec.recorded["task-1/stdout.log"])
	if !strings.Contains(stdout, "absent") {
		t.Fatalf("ambient daemon env var leaked into subprocess env: stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "PATH=") {
		t.Fatalf("expected PATH to still be passed through via the allowlist, got %q", stdout)
	}
}

// TestShellExecutor_PassesThroughGoToolchainEnv is the regression test for
// #248: a `local-ci` stage's `make ci` (-> `go build`/`go test`) must see a
// relocated Go cache/module store/proxy, not silently fall back to
// HOME-derived defaults that don't exist on a customized host.
func TestShellExecutor_PassesThroughGoToolchainEnv(t *testing.T) {
	gocache := t.TempDir()
	gomodcache := t.TempDir()
	t.Setenv("GOCACHE", gocache)
	t.Setenv("GOMODCACHE", gomodcache)
	t.Setenv("GOPROXY", "https://proxy.example.internal")

	exec, rec := newTestExecutor(t, nil)
	env := baseEnvelope(t)

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo "GOCACHE=$GOCACHE"; echo "GOMODCACHE=$GOMODCACHE"; echo "GOPROXY=$GOPROXY"`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	stdout := string(rec.recorded["task-1/stdout.log"])
	for _, want := range []string{
		"GOCACHE=" + gocache,
		"GOMODCACHE=" + gomodcache,
		"GOPROXY=https://proxy.example.internal",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("expected %q in stage stdout, got %q", want, stdout)
		}
	}
}

// TestShellExecutor_EmptyWorkspaceIsConfigError is the regression test for
// #122: exec.Cmd treats Dir == "" as "run in the current process's working
// directory" — an unset InvocationEnvelope.Workspace must fail closed as a
// configuration error instead of silently running in the daemon's own cwd.
func TestShellExecutor_EmptyWorkspaceIsConfigError(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := apiv1.InvocationEnvelope{TaskID: "task-1"} // Workspace left empty
	_, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"true"}})
	if err == nil {
		t.Fatal("expected an error for an empty Workspace")
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

// TestShellExecutor_ResultFileJSONMergedIntoOutputs proves the #132
// prNumber-handoff mechanism: a declared result file whose bytes parse as a
// flat JSON object has its string/number/bool fields merged into
// ResultEnvelope.Outputs, in addition to the file still being recorded as an
// artifact (TestShellExecutor_ResultFileLiftedToArtifact already covers
// that half).
func TestShellExecutor_ResultFileJSONMergedIntoOutputs(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "pr-result.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo '{"prNumber":"42","pull-request-url":"https://example/pr/42","draft":false}' > pr-result.json`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if result.Outputs["prNumber"] != "42" {
		t.Fatalf("outputs[prNumber] = %v, want \"42\"", result.Outputs["prNumber"])
	}
	if result.Outputs["pull-request-url"] != "https://example/pr/42" {
		t.Fatalf("outputs[pull-request-url] = %v", result.Outputs["pull-request-url"])
	}
	if result.Outputs["draft"] != false {
		t.Fatalf("outputs[draft] = %v, want false", result.Outputs["draft"])
	}
}

// TestShellExecutor_NoWorkOutputReportsResultNoWork is issue #233's core
// executor-level acceptance: a declared result file whose JSON carries
// noWork:true (OutputNoWork) reports ResultNoWork, not ResultSuccess, even
// though the command exited 0 and every other success condition held.
func TestShellExecutor_NoWorkOutputReportsResultNoWork(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "claimed-item.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo '{"claimed":false,"noWork":true}' > claimed-item.json`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultNoWork {
		t.Fatalf("status = %v, want no-work", result.Status)
	}
	if result.Outputs["claimed"] != false {
		t.Fatalf("outputs[claimed] = %v, want false", result.Outputs["claimed"])
	}
}

// TestShellExecutor_FalseNoWorkOutputIsStillSuccess is the negative control
// for TestShellExecutor_NoWorkOutputReportsResultNoWork: noWork explicitly
// false (or the key simply absent, the common case) must not accidentally
// trip the ResultNoWork path — only a literal boolean true does.
func TestShellExecutor_FalseNoWorkOutputIsStillSuccess(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "claimed-item.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo '{"id":"7","noWork":false}' > claimed-item.json`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (noWork:false must not trip ResultNoWork)", result.Status)
	}
}

// TestShellExecutor_NonzeroExitIsStillFailureNotNoWork is #233's negative
// control at the exit-code layer: a genuine command failure (a provider/auth
// error, in backlog-query's real usage) must still be ResultFailure, never
// reinterpreted as ResultNoWork just because no declared result file was
// produced — OutputNoWork is only ever consulted after exitCode==0.
func TestShellExecutor_NonzeroExitIsStillFailureNotNoWork(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `exit 1`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
}

// TestShellExecutor_ResultFileNonJSONIsNotAnError proves a declared result
// file that isn't JSON (or isn't a flat object) still satisfies the
// artifact/presence-check contract unchanged — merging Outputs is additive,
// never a new failure mode.
func TestShellExecutor_ResultFileNonJSONIsNotAnError(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "out.txt"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo 'not json' > out.txt`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if !strings.Contains(string(rec.recorded["task-1/result"]), "not json") {
		t.Fatalf("result artifact missing expected content: %v", rec.recorded["task-1/result"])
	}
}

// TestShellExecutor_NonGoobersStageOmitsRunContext proves the #322 leak
// closure at the integration level: a stage whose command is NOT the goobers
// CLI (here `sh`, standing in for local-ci's `make ci` → `go test ./...`) does
// NOT receive the run's operational identity (GOOBERS_RUN_ID/GOOBERS_WORKFLOW/
// GOOBERS_INSTANCE_ROOT) in its exec env — so, in a self-hosting project, a
// live run can't perturb its own test suite through those vars. The stage's
// own declared Task.Inputs (GOOBERS_INPUT_*) are unaffected: they are the
// stage's config, not the runner's identity, so they still flow.
func TestShellExecutor_NonGoobersStageOmitsRunContext(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	exec.InstanceRoot = "/instances/demo"
	env := baseEnvelope(t)
	env.RunID = "run-123"
	env.WorkflowID = "implementation"
	env.Inputs = map[string]interface{}{"trustLabel": "goobers:approved"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `echo "run=$GOOBERS_RUN_ID wf=$GOOBERS_WORKFLOW root=$GOOBERS_INSTANCE_ROOT input=$GOOBERS_INPUT_TRUSTLABEL"`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	got := string(rec.recorded["task-1/stdout.log"])
	// Run-context vars empty (not injected for a non-goobers command); the
	// declared input var still present.
	want := "run= wf= root= input=goobers:approved\n"
	if got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
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

// TestShellExecutor_SelfBinResolvesGoobersToken is the #229 regression control:
// a bare "goobers" command token — which a fresh stage worktree can never
// resolve, since it holds no copy of the (gitignored, uncommitted) binary — is
// rewritten to SelfBin and execs successfully; without SelfBin it fails at
// exec_start, documenting the pre-#229 behavior.
func TestShellExecutor_SelfBinResolvesGoobersToken(t *testing.T) {
	// The no-SelfBin half below observes an exec failure only if "goobers" is
	// absent from PATH; a dev machine with goobers installed would exec the real
	// binary instead. Skip in that case rather than assert against a real binary.
	if _, err := exec.LookPath("goobers"); err == nil {
		t.Skip("a real goobers is on PATH; this test isolates the SelfBin rewrite")
	}

	// A stub standing in for the goobers binary, reachable ONLY via its absolute
	// path (SelfBin), never via the "goobers" token — mirroring the worktree.
	stub := filepath.Join(t.TempDir(), "goobers-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho self-bin-marker\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	e, rec := newTestExecutor(t, nil)
	e.SelfBin = stub
	result, err := e.Run(context.Background(), baseEnvelope(t),
		apiv1.DeterministicRun{Command: []string{"goobers", "--marker"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("SelfBin should have execed the stub for the \"goobers\" token: %+v", result)
	}
	if got := string(rec.recorded["task-1/stdout.log"]); !strings.Contains(got, "self-bin-marker") {
		t.Fatalf("stub not invoked via SelfBin; stdout = %q", got)
	}

	// Directional: without SelfBin, the bare "goobers" token fails at exec.
	e2, _ := newTestExecutor(t, nil) // SelfBin unset
	result2, err := e2.Run(context.Background(), baseEnvelope(t),
		apiv1.DeterministicRun{Command: []string{"goobers", "--marker"}})
	if err != nil {
		t.Fatalf("Run (no SelfBin): %v", err)
	}
	if result2.Status != apiv1.ResultFailure || result2.Error == nil || result2.Error.Code != "exec_start" {
		t.Fatalf("without SelfBin, bare \"goobers\" must fail at exec_start: %+v", result2)
	}
}
