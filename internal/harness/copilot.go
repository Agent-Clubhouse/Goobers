package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// defaultPromptFlag is the flag CopilotAdapter passes before the rendered
// prompt text when PromptFlag is unset — `-p`/`--prompt <text>`: "Execute a
// prompt in non-interactive mode (exits after completion)" per the real CLI's
// own --help, confirmed by a live invocation while building this adapter.
const defaultPromptFlag = "-p"

// defaultExtraArgs is used when ExtraArgs is nil. --allow-all-tools is
// REQUIRED for the real CLI's non-interactive mode — without it, a session
// blocks on an interactive permission prompt instead of exiting, which would
// hang until Timeout fires. Tool-level sandboxing (restricting which tools
// Copilot may use) is deferred to V1 (SEC-044); V0's capability enforcement
// is the credential-scoping this adapter already does via EnvCapabilities.
var defaultExtraArgs = []string{"--allow-all-tools", "--log-level", "error"}

// CopilotAdapter is the V0 harness adapter for the GitHub Copilot CLI
// (GBO-040): it renders the invocation envelope + goober instructions into a
// prompt, runs the CLI non-interactively in the stage workspace with only the
// granted capabilities' credentials materialized into its environment,
// enforces the timeout, and reads back the completion file the prompt
// instructed the CLI to write.
//
// The exact CLI invocation shape is configurable rather than hardcoded
// (Command/PromptFlag/ExtraArgs) so it can be tuned without touching this
// adapter's logic, but the defaults here are verified against a real,
// installed, signed-in Copilot CLI (1.0.70) — not guessed: `copilot -p
// "<text>" --allow-all-tools --log-level error` performs the task and exits,
// confirmed by TestCopilotAdapterLiveSmoke.
type CopilotAdapter struct {
	// Command is the base CLI invocation, e.g. []string{"copilot"}.
	Command []string
	// PromptFlag precedes the rendered prompt text in the built argv.
	// Defaults to "-p" if empty.
	PromptFlag string
	// ExtraArgs are appended after the prompt flag/text. Defaults to
	// defaultExtraArgs (--allow-all-tools, required for non-interactive
	// mode) when nil; pass an empty (non-nil) slice to opt out.
	ExtraArgs []string
	// EnvCapabilities maps a declared capability name to the environment
	// variable the CLI reads its credential from, e.g.
	// {"repo:push": "GH_TOKEN"}. Only capabilities present here — and
	// present in the invocation's declared+granted set — ever reach the
	// subprocess environment (capability enforcement, GBO-052).
	EnvCapabilities map[string]string
	// Runner executes the subprocess; defaults to ExecProcessRunner.
	Runner ProcessRunner
	// VersionArgs are the args used to preflight-check the CLI responds
	// (default {"--version"}).
	VersionArgs []string
}

// Name returns the adapter's registry name.
func (c *CopilotAdapter) Name() string { return "copilot-cli" }

// Preflight verifies the Copilot CLI binary is on PATH and responds to a
// version check, returning an actionable error otherwise (GBO-011; wired into
// `goobers validate --check-harness`).
func (c *CopilotAdapter) Preflight(ctx context.Context) error {
	if len(c.Command) == 0 {
		return fmt.Errorf("harness: copilot-cli: no command configured")
	}
	bin := c.Command[0]
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("harness: copilot-cli: %q not found on PATH — install the GitHub Copilot CLI "+
			"and sign in before running agentic stages", bin)
	}
	args := c.VersionArgs
	if len(args) == 0 {
		args = []string{"--version"}
	}
	res, err := c.runner().Run(ctx, ProcessRequest{Command: append([]string{bin}, args...)})
	if err != nil {
		return fmt.Errorf("harness: copilot-cli: %q did not respond to %v: %w — check it is installed and signed in", bin, args, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("harness: copilot-cli: %q %v exited %d — check it is installed and signed in", bin, args, res.ExitCode)
	}
	return nil
}

func (c *CopilotAdapter) runner() ProcessRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecProcessRunner{}
}

// Run renders the prompt, runs the CLI non-interactively in req.Workspace
// with capability-scoped credentials in its environment, and reads back the
// completion file at req.CompletionPath.
func (c *CopilotAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	if len(c.Command) == 0 {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: no command configured")
	}

	prompt := renderPrompt(req)
	// Also write the rendered prompt to the workspace for human debugging —
	// the CLI itself receives it inline (its -p/--prompt flag takes text,
	// not a file path).
	debugPath := filepath.Join(req.Workspace, ".goobers", "prompt.md")
	if err := os.MkdirAll(filepath.Dir(debugPath), 0o755); err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: prepare prompt dir: %w", err)
	}
	if err := os.WriteFile(debugPath, []byte(prompt), 0o644); err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: write prompt: %w", err)
	}

	flag := c.PromptFlag
	if flag == "" {
		flag = defaultPromptFlag
	}
	extra := c.ExtraArgs
	if extra == nil {
		extra = defaultExtraArgs
	}
	argv := append(append([]string{}, c.Command...), flag, prompt)
	argv = append(argv, extra...)

	env, err := c.credentialEnv(ctx, req)
	if err != nil {
		return Outcome{}, err
	}

	result, err := c.runner().Run(ctx, ProcessRequest{
		Command: argv,
		Dir:     req.Workspace,
		Env:     env,
		Timeout: req.Timeout,
	})
	out := Outcome{Transcript: result.Transcript}
	if err != nil {
		return out, err
	}

	payload, err := readCompletion(req.Workspace, req.CompletionPath)
	if err != nil {
		return out, err
	}
	out.Payload = payload
	return out, nil
}

// passthroughVars are the only ambient daemon-process env vars carried into
// the harness subprocess — never the full os.Environ(). PATH/HOME/TMPDIR let
// the CLI (and its own signed-in session storage, which lives under HOME)
// and anything it shells out to find their toolchain; none carries secret
// material. Mirrors internal/executor's identical SEC-045 allowlist so both
// executors give the same guarantee: no ambient credential a stage didn't
// declare ever reaches a stage's process, even if the daemon's own
// environment happens to hold one (e.g. a resolver-sourced token env var
// from instance.yaml). Verified against the real Copilot CLI: it functions
// correctly under exactly this allowlist plus its own auth env var.
var passthroughVars = []string{"PATH", "HOME", "TMPDIR"}

// baseEnv returns the minimal, explicit env every harness process starts
// with: the passthrough allowlist carried forward from the daemon process,
// and nothing else.
func baseEnv() []string {
	env := make([]string, 0, len(passthroughVars))
	for _, name := range passthroughVars {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// credentialEnv builds the subprocess environment: baseEnv() (PATH/HOME/
// TMPDIR — never a secret store, never the full os.Environ()) plus exactly
// the capability tokens this adapter is configured to inject and that were
// actually declared for this invocation. A capability this adapter is
// configured to inject but that fails to resolve is a hard stop — the
// harness never runs half-credentialed.
func (c *CopilotAdapter) credentialEnv(ctx context.Context, req RunRequest) ([]string, error) {
	env := baseEnv()
	for _, capability := range req.Envelope.Capabilities {
		envVar, ok := c.EnvCapabilities[capability]
		if !ok {
			continue
		}
		token, err := req.Credentials.Token(ctx, capability)
		if err != nil {
			return nil, fmt.Errorf("harness: copilot-cli: resolve %s: %w", capability, err)
		}
		env = append(env, envVar+"="+token)
	}
	return env, nil
}
