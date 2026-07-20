package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/telemetry"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

type copilotModelCapabilities struct {
	longContext     bool
	reasoningEffort map[string]struct{}
}

func copilotEfforts(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

var copilotModels = map[string]copilotModelCapabilities{
	"auto":                 {},
	"claude-fable-5":       {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-sonnet-5":      {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-sonnet-4.6":    {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "max")},
	"claude-sonnet-4.5":    {},
	"claude-haiku-4.5":     {},
	"claude-opus-4.8-fast": {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-opus-4.8":      {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-opus-4.7":      {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-opus-4.6":      {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "max")},
	"claude-opus-4.5":      {},
	"gpt-5.6-sol":          {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"gpt-5.6-terra":        {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"gpt-5.6-luna":         {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"gpt-5.5":              {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh")},
	"gpt-5.4":              {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh")},
	"gpt-5.3-codex":        {reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh")},
	"gpt-5.4-mini":         {reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh")},
	"gpt-5-mini":           {reasoningEffort: copilotEfforts("none", "low", "medium", "high")},
	"gemini-3.1-pro-preview": {longContext: true,
		reasoningEffort: copilotEfforts("none", "low", "medium", "high")},
	"gemini-3.5-flash": {longContext: true,
		reasoningEffort: copilotEfforts("none", "minimal", "low", "medium", "high")},
	"kimi-k2.7-code":   {},
	"mai-code-1-flash": {},
}

// CopilotAdapter is the V0 harness adapter for the GitHub Copilot CLI
// (GBO-040): it renders the invocation envelope + goober instructions into a
// prompt, runs the CLI non-interactively in the stage workspace with only the
// granted capabilities' credentials materialized into its environment,
// captures supported native session events when available, enforces the
// timeout, and reads back the completion file the prompt instructed the CLI to
// write.
//
// The exact CLI invocation shape is configurable rather than hardcoded
// (Command/PromptFlag/ExtraArgs) so it can be tuned without touching this
// adapter's logic, but the defaults here are verified against a real,
// installed, signed-in Copilot CLI (1.0.71) — not guessed: `copilot -p
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
	// AuthCheckArgs, if non-empty, are run as a second preflight probe after
	// VersionArgs to detect a signed-OUT CLI: `--version` succeeds even when the
	// user is not authenticated (GBO-011, #238), so a version check alone can't
	// catch a signed-out session — the failure would instead surface mid-run as
	// a burned agentic attempt. A non-zero exit (or runner error) from this probe
	// fails preflight with an actionable sign-in message. Empty by default: the
	// exact non-interactive auth/status invocation the real Copilot CLI offers is
	// wired at the composition root once confirmed, so a wrong guess can't
	// falsely refuse to start every agentic run.
	AuthCheckArgs []string
}

// Name returns the adapter's registry name.
func (c *CopilotAdapter) Name() string { return "copilot-cli" }

// ValidateConfig rejects model and option values the Copilot CLI adapter does
// not know how to express. This is called during config admission.
func (c *CopilotAdapter) ValidateConfig(model string, options map[string]apiextensionsv1.JSON) error {
	_, err := normalizeCopilotConfig(model, options)
	return err
}

func normalizeCopilotConfig(model string, options map[string]apiextensionsv1.JSON) (map[string]string, error) {
	var capabilities copilotModelCapabilities
	if model != "" {
		var ok bool
		capabilities, ok = copilotModels[model]
		if !ok {
			return nil, fmt.Errorf("unknown model %q", model)
		}
	}
	names := make([]string, 0, len(options))
	for name := range options {
		names = append(names, name)
	}
	sort.Strings(names)
	normalized := make(map[string]string, len(options))
	for _, name := range names {
		if name != "context" && name != "reasoningEffort" {
			return nil, fmt.Errorf("unknown harness option %q", name)
		}
		var value string
		if err := json.Unmarshal(options[name].Raw, &value); err != nil {
			return nil, fmt.Errorf("harness option %q must be a string: %w", name, err)
		}
		switch name {
		case "context":
			if value != "default" && value != "long_context" {
				return nil, fmt.Errorf("invalid context value %q", value)
			}
			if value == "long_context" {
				if model == "" {
					return nil, fmt.Errorf("context value %q requires an explicit model", value)
				}
				if !capabilities.longContext {
					return nil, fmt.Errorf("context value %q is not supported by model %q", value, model)
				}
			}
		case "reasoningEffort":
			if model == "" {
				return nil, fmt.Errorf("reasoningEffort requires an explicit model")
			}
			if _, ok := capabilities.reasoningEffort[value]; !ok {
				return nil, fmt.Errorf("reasoningEffort value %q is not supported by model %q", value, model)
			}
		}
		normalized[name] = value
	}
	return normalized, nil
}

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
	// Explicit baseEnv(), not the ProcessRequest zero value — since #122,
	// ExecProcessRunner treats a nil Env as NO environment (SEC-045
	// default-deny), so the version-check subprocess needs this passed
	// explicitly the same way Run's credentialEnv does.
	res, err := c.runner().Run(ctx, ProcessRequest{Command: append([]string{bin}, args...), Env: baseEnv()})
	if err != nil {
		return fmt.Errorf("harness: copilot-cli: %q did not respond to %v: %w — check it is installed and signed in", bin, args, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("harness: copilot-cli: %q %v exited %d — check it is installed and signed in", bin, args, res.ExitCode)
	}
	// A signed-out CLI passes --version but can't do agentic work, so probe
	// authentication too when configured (GBO-011, #238) — catching it here at
	// startup rather than as a burned mid-run agentic attempt.
	if len(c.AuthCheckArgs) > 0 {
		res, err := c.runner().Run(ctx, ProcessRequest{Command: append([]string{bin}, c.AuthCheckArgs...), Env: baseEnv()})
		if err != nil {
			return fmt.Errorf("harness: copilot-cli: %q %v (sign-in check) failed: %w — run the Copilot CLI and sign in", bin, c.AuthCheckArgs, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("harness: copilot-cli: %q %v (sign-in check) exited %d — the CLI appears signed out; run the Copilot CLI and sign in", bin, c.AuthCheckArgs, res.ExitCode)
		}
	}
	return nil
}

func (c *CopilotAdapter) runner() ProcessRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecProcessRunner{}
}

// Run renders the prompt, runs the CLI non-interactively in req.Workspace with
// capability-scoped credentials in its environment, prefers converted native
// session events over the subprocess transcript when available, and reads back
// the completion file at req.CompletionPath.
func (c *CopilotAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	if len(c.Command) == 0 {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: no command configured")
	}
	if req.Workspace == "" {
		// exec.Cmd treats Dir == "" as "run in the daemon's own working
		// directory" — a silent, surprising fallback (#122) rather than the
		// fail-closed misconfiguration error an unset workspace should be.
		return Outcome{}, fmt.Errorf("harness: copilot-cli: RunRequest.Workspace is empty")
	}
	harnessOptions, err := normalizeCopilotConfig(req.Model, req.HarnessOptions)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: invalid configuration: %w", err)
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
	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	if value, ok := harnessOptions["context"]; ok {
		argv = append(argv, "--context", value)
	}
	if value, ok := harnessOptions["reasoningEffort"]; ok {
		argv = append(argv, "--reasoning-effort", value)
	}
	argv = append(argv, extra...)

	env, err := c.credentialEnv(ctx, req)
	if err != nil {
		return Outcome{}, err
	}
	nativeTranscriptPath := ""
	if !copilotCommandSelectsSession(argv) {
		// Pin the log to this run without replacing the home that also holds
		// the user's Copilot configuration.
		if copilotHome, ok := copilotConfigHome(env); ok {
			captureID, err := newCopilotCaptureID()
			if err != nil {
				return Outcome{}, fmt.Errorf("harness: copilot-cli: create transcript capture id: %w", err)
			}
			argv = append(argv, "--session-id", captureID)
			nativeTranscriptPath = copilotSessionLogPath(copilotHome, captureID)
		}
	}

	result, err := c.runner().Run(ctx, ProcessRequest{
		Command:            argv,
		Dir:                req.Workspace,
		Env:                env,
		Timeout:            req.Timeout,
		MaxTranscriptBytes: req.MaxTranscriptBytes,
	})
	out := Outcome{
		Transcript:             result.Transcript,
		TranscriptTruncated:    result.TranscriptTruncated,
		TranscriptDroppedBytes: result.TranscriptDroppedBytes,
	}
	if nativeTranscriptPath != "" {
		if native, ok := readCopilotSessionTranscript(nativeTranscriptPath, req.MaxTranscriptBytes); ok {
			out.Transcript = native.data
			out.TranscriptTruncated = native.truncated
			out.TranscriptDroppedBytes = native.droppedBytes
		}
	}
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

// baseEnv returns the minimal, explicit env every harness process starts
// with — internal/procenv.BaseEnv(), the allowlist internal/executor's
// baseEnv() shares (#248, closing the #98/#122 drift for good: one
// definition instead of two hand-kept-in-sync copies).
func baseEnv() []string {
	return procenv.BaseEnv()
}

// credentialEnv builds the subprocess environment: baseEnv() (PATH/HOME/
// TMPDIR — never a secret store, never the full os.Environ()), the stage
// telemetry directory, and exactly the capability tokens this adapter is
// configured to inject and that were actually declared for this invocation.
// A configured capability that fails to resolve is a hard stop — the harness
// never runs half-credentialed.
func (c *CopilotAdapter) credentialEnv(ctx context.Context, req RunRequest) ([]string, error) {
	env := baseEnv()
	telemetryDir := req.TelemetryDir
	if telemetryDir == "" {
		telemetryDir = telemetry.PrepareStageTelemetryDir(req.Workspace)
	}
	if telemetryDir != "" {
		env = append(env, telemetry.StageTelemetryEnv+"="+telemetryDir)
	}
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
