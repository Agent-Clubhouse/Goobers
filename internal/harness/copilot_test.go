package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/procenv"
)

// fakeProcessRunner is a scripted ProcessRunner double: it lets tests inspect
// the built command/env/dir and script an arbitrary side effect (e.g. writing
// the completion file, as a real CLI would) without a real subprocess.
type fakeProcessRunner struct {
	lastReq ProcessRequest
	act     func(req ProcessRequest) error
	result  ProcessResult
	err     error
}

func (f *fakeProcessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	f.lastReq = req
	if f.act != nil {
		if err := f.act(req); err != nil {
			return f.result, err
		}
	}
	return f.result, f.err
}

// pushCredentials builds a *credentials.Set materialized for "repo:push",
// backed by a real env-var token ref, for tests exercising credential
// injection into the CLI subprocess.
func pushCredentials(t *testing.T, capability, token string) *credentials.Set {
	t.Helper()
	t.Setenv("PUSH_TOKEN_ENV", token)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "push-ref", Env: "PUSH_TOKEN_ENV"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: capability, Ref: "push-ref"}}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	set, err := injector.Materialize(context.Background(), []string{capability})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return set
}

func TestCopilotAdapterRendersPromptAndCollectsResult(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("copilot: implementing...\ncopilot: done."), ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "ok"})
		},
	}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		Runner:          runner,
		EnvCapabilities: map[string]string{"repo:push": "GH_TOKEN"},
	}

	creds := pushCredentials(t, "repo:push", "push-token-value")
	env := testEnvelope(workspace, "repo:push")
	req := RunRequest{
		Mode:           ModeInvoke,
		Envelope:       env,
		Instructions:   "You are a coder.",
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
		Timeout:        5 * time.Second,
	}

	out, err := adapter.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.Payload) == 0 {
		t.Fatal("expected a non-empty result payload")
	}
	if string(out.Transcript) != "copilot: implementing...\ncopilot: done." {
		t.Fatalf("transcript = %q", out.Transcript)
	}

	// The command was built as Command + PromptFlag + prompt text + extras.
	if len(runner.lastReq.Command) < 3 || runner.lastReq.Command[0] != "copilot" || runner.lastReq.Command[1] != defaultPromptFlag {
		t.Fatalf("unexpected command: %v", runner.lastReq.Command)
	}
	promptText := runner.lastReq.Command[2]
	if !strings.Contains(promptText, "You are a coder.") {
		t.Fatalf("prompt missing instructions: %q", promptText)
	}
	if !strings.Contains(promptText, req.Envelope.Goal) {
		t.Fatalf("prompt missing goal: %q", promptText)
	}
	if !strings.Contains(promptText, DefaultResultPath) {
		t.Fatalf("prompt missing completion path directive: %q", promptText)
	}
	found := false
	for _, arg := range runner.lastReq.Command {
		if arg == "--allow-all-tools" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --allow-all-tools in default extra args: %v", runner.lastReq.Command)
	}
	// The prompt is also written to the workspace for human debugging.
	debugPrompt, err := os.ReadFile(filepath.Join(workspace, ".goobers", "prompt.md"))
	if err != nil {
		t.Fatalf("read debug prompt: %v", err)
	}
	if string(debugPrompt) != promptText {
		t.Fatalf("debug prompt file does not match the prompt sent to the CLI")
	}

	// The credential was injected as an env var, not a CLI arg.
	foundEnv := false
	for _, kv := range runner.lastReq.Env {
		if kv == "GH_TOKEN=push-token-value" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Fatalf("expected GH_TOKEN=push-token-value in subprocess env, got %v", runner.lastReq.Env)
	}
	for _, arg := range runner.lastReq.Command {
		if strings.Contains(arg, "push-token-value") {
			t.Fatalf("token leaked into argv: %v", runner.lastReq.Command)
		}
	}
}

func TestCopilotAdapterUndeclaredCapabilityNeverResolved(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		Runner:          runner,
		EnvCapabilities: map[string]string{"repo:push": "GH_TOKEN"},
	}

	// Credentials materialized for "repo:read" only — "repo:push" was never
	// declared, so the adapter must not (and per credentials.Set, cannot)
	// resolve or inject it.
	creds := pushCredentials(t, "repo:read", "irrelevant")
	env := testEnvelope(workspace, "repo:read")
	req := RunRequest{
		Envelope:       env,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, "GH_TOKEN=") {
			t.Fatalf("undeclared capability's token leaked into env: %v", runner.lastReq.Env)
		}
	}
}

// TestCopilotAdapterDoesNotPassthroughAmbientDaemonEnv is the regression test
// for the QA finding on PR #70: the subprocess must not inherit the daemon
// process's own environment wholesale (os.Environ()), since that would leak
// any resolver-sourced credential env var (e.g. instance.yaml's
// token.env — GOOBERS_GITHUB_TOKEN) into every stage regardless of whether it
// declared the corresponding capability (SEC-045, GBO-052).
func TestCopilotAdapterDoesNotPassthroughAmbientDaemonEnv(t *testing.T) {
	const ambientSecretVar = "GOOBERS_GITHUB_TOKEN"
	t.Setenv(ambientSecretVar, "ambient-daemon-secret-never-declared")

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	// No capabilities declared at all — the stage asked for nothing.
	env := testEnvelope(workspace)
	req := RunRequest{
		Envelope:       env,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, ambientSecretVar+"=") {
			t.Fatalf("ambient daemon env var leaked into subprocess env: %v", runner.lastReq.Env)
		}
	}
	// The allowlist itself (PATH at minimum) should still be present, so the
	// fix isn't accidentally starving the CLI of what it needs to run.
	foundPath := false
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, "PATH=") {
			foundPath = true
		}
	}
	if !foundPath {
		t.Fatalf("expected PATH to still be passed through via the allowlist, got %v", runner.lastReq.Env)
	}
}

// TestCopilotAdapterPassesThroughExtendedAllowlist is the regression test for
// #75: the well-known, non-secret env conventions diverse tier-1 hosts rely
// on (XDG base dirs, locale, TLS/proxy config) must reach the subprocess, not
// just PATH/HOME/TMPDIR.
func TestCopilotAdapterPassesThroughExtendedAllowlist(t *testing.T) {
	extended := map[string]string{
		"XDG_CONFIG_HOME": "/home/tester/.config",
		"XDG_DATA_HOME":   "/home/tester/.local/share",
		"LANG":            "en_US.UTF-8",
		"LC_ALL":          "C",
		"LC_CTYPE":        "en_US.UTF-8",
		"SSL_CERT_FILE":   "/etc/ssl/certs/custom-ca.pem",
		"HTTP_PROXY":      "http://proxy.example.internal:8080",
		"HTTPS_PROXY":     "https://proxy.example.internal:8443",
		"NO_PROXY":        "localhost,127.0.0.1",
	}
	for name, value := range extended {
		t.Setenv(name, value)
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := make(map[string]bool, len(extended))
	for name, value := range extended {
		want := name + "=" + value
		for _, kv := range runner.lastReq.Env {
			if kv == want {
				got[name] = true
			}
		}
	}
	for name := range extended {
		if !got[name] {
			t.Fatalf("%s did not pass through into subprocess env, got %v", name, runner.lastReq.Env)
		}
	}
}

// TestCopilotAdapterExtendedAllowlistStillBlocksSecretShapedVars proves the
// #75 extension stays default-deny: an ambient var that merely resembles an
// allowlisted name (shares a prefix substring) or looks like a credential
// must not pass, only exact allowlisted names and the LC_* family do.
func TestCopilotAdapterExtendedAllowlistStillBlocksSecretShapedVars(t *testing.T) {
	blocked := map[string]string{
		// Secret-shaped, unrelated to the allowlist.
		"AWS_SECRET_ACCESS_KEY": "not-a-real-secret-but-should-never-pass",
		// Shares the "LANG" substring as a prefix but is a distinct var name —
		// would leak if baseEnv used strings.HasPrefix(name, "LANG") instead
		// of an exact match.
		"LANGUAGE_MODEL_API_KEY": "should-not-pass-either",
		// Shares "LC_" as a substring but not as a prefix — must not match
		// the LC_* family.
		"LOCALE_LC_OVERRIDE_SECRET": "should-not-pass",
	}
	for name, value := range blocked {
		t.Setenv(name, value)
	}
	t.Setenv("LANG", "en_US.UTF-8") // the real, exact allowlisted name

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for name := range blocked {
		for _, kv := range runner.lastReq.Env {
			if strings.HasPrefix(kv, name+"=") {
				t.Fatalf("blocked var %s leaked into subprocess env: %v", name, runner.lastReq.Env)
			}
		}
	}
	foundLang := false
	for _, kv := range runner.lastReq.Env {
		if kv == "LANG=en_US.UTF-8" {
			foundLang = true
		}
	}
	if !foundLang {
		t.Fatalf("expected the exact allowlisted LANG to still pass through, got %v", runner.lastReq.Env)
	}
}

// TestBaseEnvMatchesProcenv is the #248 drift-guard: harness's baseEnv()
// must be exactly procenv.BaseEnv() — the shared definition executor's
// baseEnv() also delegates to — not a local copy that can silently diverge
// again the way #98/#122 did.
func TestBaseEnvMatchesProcenv(t *testing.T) {
	t.Setenv("GOMODCACHE", "/custom/gomodcache")
	t.Setenv("LC_ALL", "C")

	got := append([]string(nil), baseEnv()...)
	want := append([]string(nil), procenv.BaseEnv()...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("harness baseEnv() diverged from procenv.BaseEnv():\n got:  %v\n want: %v", got, want)
	}
}

// TestCopilotAdapterEmptyWorkspaceIsConfigError is the regression test for
// #122: exec.Cmd treats Dir == "" as "run in the current process's working
// directory" — an unset RunRequest.Workspace must fail closed as a
// configuration error instead of silently running in the daemon's own cwd.
func TestCopilotAdapterEmptyWorkspaceIsConfigError(t *testing.T) {
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}}
	_, err := adapter.Run(context.Background(), RunRequest{CompletionPath: DefaultResultPath}) // Workspace left empty
	if err == nil {
		t.Fatal("expected an error for an empty Workspace")
	}
}

func TestCopilotAdapterFailsClosedOnMissingCommand(t *testing.T) {
	adapter := &CopilotAdapter{}
	if err := adapter.Preflight(context.Background()); err == nil {
		t.Fatal("expected Preflight to fail with no command configured")
	}
	_, err := adapter.Run(context.Background(), RunRequest{Workspace: t.TempDir(), CompletionPath: DefaultResultPath})
	if err == nil {
		t.Fatal("expected Run to fail with no command configured")
	}
}

func TestCopilotAdapterPreflightMissingBinary(t *testing.T) {
	adapter := &CopilotAdapter{Command: []string{"definitely-not-a-real-copilot-cli-binary"}}
	err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected Preflight to fail for a binary not on PATH")
	}
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("error = %v, want an actionable PATH message", err)
	}
}

func TestCopilotAdapterPreflightSucceeds(t *testing.T) {
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner}
	if err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
}

func TestCopilotAdapterPreflightNonZeroExit(t *testing.T) {
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 1}}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner}
	err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected Preflight to fail on non-zero exit")
	}
}

func TestExecProcessRunnerTimeout(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	runner := ExecProcessRunner{}
	_, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sleep", "5"},
		Timeout: 50 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
}

// TestExecProcessRunnerKillsProcessGroup is the regression test for #119: a
// background grandchild that stays in the same process group (the common
// case — job control off) must die with the timeout kill, not survive and
// keep the harness stage's stdout pipe open.
func TestExecProcessRunnerKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	runner := ExecProcessRunner{}
	start := time.Now()
	_, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sh", "-c", "sleep 30 & wait"},
		Timeout: 100 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run took %s, want well under the 30s sleep — process group was not killed", elapsed)
	}
}

// TestExecProcessRunnerTimeoutGivesUpOnEscapedDescendant is the regression
// test for #119's WaitDelay gap: a grandchild that escapes the process
// group (via job control's own new-pgid-per-background-job behavior, the
// portable stand-in for setsid) survives the group kill and keeps the
// stdout pipe open, so cmd.Wait() would never return on its own. Run must
// still return within groupKillWaitDelay of the timeout rather than hanging
// for the escaped process's full lifetime.
func TestExecProcessRunnerTimeoutGivesUpOnEscapedDescendant(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	runner := ExecProcessRunner{}
	start := time.Now()
	// `set -m` gives the backgrounded sleep its own process group — it
	// outlives bash's own near-immediate exit and is never reached by the
	// group kill (bash's group, not its own). 30s comfortably exceeds
	// groupKillWaitDelay (5s), so the test can only pass via the give-up
	// bound, not by the escaped process happening to exit on its own first.
	_, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"bash", "-c", "set -m; sleep 30 & sleep 0.1"},
		Timeout: 100 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("Run took %s, want under ~%s (timeout + groupKillWaitDelay) — the give-up bound did not engage", elapsed, 100*time.Millisecond+groupKillWaitDelay)
	}
}

// TestExecProcessRunnerDefaultsToEmptyEnv is the regression test for #122:
// os/exec treats a nil Cmd.Env as "inherit this process's environment" —
// ExecProcessRunner must not let that fail-open default through when a
// caller leaves ProcessRequest.Env unset.
func TestExecProcessRunnerDefaultsToEmptyEnv(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	const ambientSecretVar = "GOOBERS_AMBIENT_DAEMON_SECRET"
	t.Setenv(ambientSecretVar, "ambient-daemon-secret-never-declared")

	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sh", "-c", `test -z "$` + ambientSecretVar + `" && echo absent`},
		// Env deliberately left unset.
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Transcript), "absent") {
		t.Fatalf("ambient daemon env var leaked with Env unset: transcript = %q", res.Transcript)
	}
}

func TestExecProcessRunnerCapturesTranscript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sh", "-c", "echo hello-stdout; echo hello-stderr 1>&2"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Transcript), "hello-stdout") || !strings.Contains(string(res.Transcript), "hello-stderr") {
		t.Fatalf("transcript = %q", res.Transcript)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestCopilotAdapterLiveSmoke performs a trivial edit task against a fixture
// workspace using the real, installed Copilot CLI. Skipped unless both
// GOOBERS_COPILOT_LIVE_SMOKE=1 is set and a `copilot` binary is on PATH — CI
// exercises the fake-harness path instead; this is the acceptance
// criterion's "Live smoke test behind an env flag."
func TestCopilotAdapterLiveSmoke(t *testing.T) {
	if os.Getenv("GOOBERS_COPILOT_LIVE_SMOKE") != "1" {
		t.Skip("set GOOBERS_COPILOT_LIVE_SMOKE=1 to run against a real, signed-in Copilot CLI")
	}
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skip("copilot CLI not found on PATH")
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "GREETING.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("seed fixture file: %v", err)
	}

	adapter := &CopilotAdapter{Command: []string{"copilot"}}
	if err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, nil, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	creds, err := injector.Materialize(context.Background(), nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	env := testEnvelope(workspace)
	env.Goal = "Append the word 'world' to GREETING.txt, then write your result envelope as instructed."
	req := RunRequest{
		Mode:           ModeInvoke,
		Envelope:       env,
		Instructions:   "You are a coder goober performing a trivial smoke-test edit.",
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
		Timeout:        2 * time.Minute,
	}
	out, err := adapter.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v (transcript: %s)", err, out.Transcript)
	}
	if len(out.Payload) == 0 {
		t.Fatal("expected a completion payload from the live CLI")
	}
}

// TestCopilotAdapterPreflightSignedOutFailsAuthProbe is the #238 control: a CLI
// that passes --version but fails the configured auth probe (signed out) fails
// preflight — the case a version-only check misses (GBO-011).
func TestCopilotAdapterPreflightSignedOutFailsAuthProbe(t *testing.T) {
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0}, // --version succeeds
		act: func(req ProcessRequest) error {
			for _, a := range req.Command {
				if a == "auth" { // the auth probe fails: signed out
					return errors.New("not signed in")
				}
			}
			return nil
		},
	}
	adapter := &CopilotAdapter{Command: []string{"echo"}, AuthCheckArgs: []string{"auth", "status"}, Runner: runner}
	err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected preflight to fail when the sign-in probe fails")
	}
	if !strings.Contains(err.Error(), "sign") {
		t.Fatalf("error should be an actionable sign-in message: %v", err)
	}
}

// TestCopilotAdapterPreflightSignedInPasses confirms preflight passes when both
// --version and the configured auth probe succeed.
func TestCopilotAdapterPreflightSignedInPasses(t *testing.T) {
	adapter := &CopilotAdapter{
		Command:       []string{"echo"},
		AuthCheckArgs: []string{"auth", "status"},
		Runner:        &fakeProcessRunner{result: ProcessResult{ExitCode: 0}},
	}
	if err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight should pass when signed in: %v", err)
	}
}

// TestCopilotAdapterPreflightNoAuthProbeByDefault confirms that with no
// AuthCheckArgs configured, preflight does not run (or require) an auth probe —
// so the version-only path is unchanged until a real auth command is wired.
func TestCopilotAdapterPreflightNoAuthProbeByDefault(t *testing.T) {
	calls := 0
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act:    func(ProcessRequest) error { calls++; return nil },
	}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner} // no AuthCheckArgs
	if err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one probe (--version), got %d — no auth probe should run by default", calls)
	}
}
