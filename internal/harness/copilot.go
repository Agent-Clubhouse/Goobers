package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/capability"
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
// hang until Timeout fires. Run separately restricts the tools visible to the
// model when the goober declares a non-empty allowlist.
//
// --log-level MUST be "all" (not "error"): Copilot CLI 1.0.76-2 regressed so
// that any --log-level value except "all" makes the process exit 1 even on a
// fully successful prompt (hasError=false, clean shutdown). Passing "error"
// here made every agentic Run() exit 1, failing the implement/review stages
// while the untouched auth probe (no --log-level) kept passing. Reproduced:
// `copilot -p "…" --allow-all-tools --available-tools= --log-level error` -> 1,
// same with --log-level all (or omitted) -> 0.
var defaultExtraArgs = []string{"--allow-all-tools", "--log-level", "all"}

const fallbackToDefaultOption = "fallback-to-default"

const copilotModelDiscoveryTimeout = 30 * time.Second

// errModelDiscoveryDeferred reports that DeferDiscovery skipped a cold-cache
// model discovery. It flows into resolveConfig's existing discovery-error
// branch, so the model is accepted unverified with a warning (#3336).
var errModelDiscoveryDeferred = errors.New("model discovery deferred at daemon startup (#3336); the harness verifies the model at run time")

const copilotModelPolicyDisabled = "disabled"

// Shipped goobers declare harness-neutral tool groups. Copilot filters the
// concrete model-facing tool IDs within those groups.
var copilotToolGroups = map[string][]string{
	"shell": {
		"bash", "read_bash", "stop_bash", "list_bash",
		"powershell", "read_powershell", "stop_powershell", "list_powershell",
		"view", "create", "edit", "str_replace_editor", "apply_patch",
		"grep", "rg", "glob",
	},
	"github": {
		"github-mcp-server-get_copilot_space",
		"github-mcp-server-get_file_contents",
		"github-mcp-server-list_copilot_spaces",
		"github-mcp-server-search_code",
		"github-mcp-server-search_repositories",
		"github-mcp-server-search_users",
		"github-mcp-server-add_issue_comment",
		"github-mcp-server-get_label",
		"github-mcp-server-issue_read",
		"github-mcp-server-issue_write",
		"github-mcp-server-list_issue_fields",
		"github-mcp-server-list_issue_types",
		"github-mcp-server-list_issues",
		"github-mcp-server-search_issues",
		"github-mcp-server-sub_issue_write",
	},
	"telemetry": {
		"view", "grep", "rg", "glob",
	},
}

type copilotModelCapabilities struct {
	longContext     bool
	reasoningEffort map[string]struct{}
}

// CopilotModelInfo is the model metadata needed to validate adapter options.
type CopilotModelInfo struct {
	ID                        string
	PolicyState               string
	SupportedReasoningEfforts []string
}

// CopilotModelLister queries the installed, authenticated Copilot runtime.
type CopilotModelLister interface {
	ListModels(ctx context.Context, command, env []string) ([]CopilotModelInfo, error)
}

// models.list does not expose context-tier support. This table is used only to
// validate that optional flag; model availability always comes from ModelLister.
var copilotLongContextModels = map[string]bool{
	"claude-fable-5":         true,
	"claude-sonnet-5":        true,
	"claude-sonnet-4.6":      true,
	"claude-opus-4.8-fast":   true,
	"claude-opus-4.8":        true,
	"claude-opus-4.7":        true,
	"claude-opus-4.6":        true,
	"gpt-5.6-sol":            true,
	"gpt-5.6-terra":          true,
	"gpt-5.6-luna":           true,
	"gpt-5.5":                true,
	"gpt-5.4":                true,
	"gemini-3.1-pro-preview": true,
	"gemini-3.5-flash":       true,
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
	// OptionalCredentialCapabilities names capabilities whose credential may
	// be omitted because the CLI can use an existing authenticated user session.
	// A configured grant still resolves and injects normally; only the absence
	// of a grant is tolerated. Other capabilities remain fail-closed.
	OptionalCredentialCapabilities map[string]bool
	// Runner executes the subprocess; defaults to ExecProcessRunner.
	Runner ProcessRunner
	// ModelLister discovers models from the authenticated Copilot runtime.
	// Defaults to the official Copilot SDK.
	ModelLister CopilotModelLister
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
	// ExtraEnvAllowlist names additional ambient env vars carried into the
	// harness subprocess (and its preflight probes) on top of the built-in
	// procenv default-deny allowlist — the instance's RunnerConfig.EnvPassthrough
	// (#736), kept in lockstep with the executor's identical extension so a
	// toolchain env var a `dotnet`/`cargo` agentic stage needs is visible to the
	// harness too. Empty by default: the built-in allowlist, unchanged.
	ExtraEnvAllowlist []string
	// InstanceRoot is exposed to the agentic subprocess so a goobers CLI command
	// it invokes can resolve instance configuration outside the stage worktree.
	InstanceRoot string
	// SelfBin is the running daemon's executable path. It is exposed to the
	// agentic subprocess so tools can invoke that exact goobers binary.
	SelfBin string
	// DeferDiscovery makes a cold-cache ResolveConfig skip model discovery
	// entirely instead of spawning the Copilot CLI and waiting on its JSON-RPC
	// handshake. The daemon sets this for startup admission (#3336): in an
	// environment where the CLI cannot complete its handshake, the spawned
	// child can exhaust the pod's memory cgroup before the discovery timeout
	// fires, and the OOM killer takes the daemon down with it — a boot loop no
	// in-process deadline can prevent. Deferred resolution takes the existing
	// accept-unverified path (a warning, not an error); a warm cache is still
	// served. Interactive callers (validate, status) keep discovery on, where
	// verifying model names against the live CLI is the point.
	DeferDiscovery bool

	modelsMu        sync.Mutex
	availableModels map[string]copilotModelCapabilities
	// modelsErr negative-caches a failed discovery for this adapter's lifetime.
	// Admission resolves every goober through one adapter, and without this a
	// single unreachable CLI is re-spawned once PER GOOBER: measured in #3336
	// as a fresh ~295MB CLI process every ~2.5s (the SDK's ForceStop kills the
	// node wrapper but not the copilot binary it spawned, so each attempt's
	// grandchild survives) — 14 goobers were enough to OOM a 4Gi pod before
	// the single-attempt timeout ever fired. One spawn per adapter instance;
	// registries are rebuilt per admission pass, so a reload retries cleanly.
	modelsErr error
}

// Name returns the adapter's registry name.
func (c *CopilotAdapter) Name() string { return "copilot-cli" }

// ValidateConfig rejects model and option values the Copilot CLI adapter does
// not know how to express. This is called during config admission.
func (c *CopilotAdapter) ValidateConfig(model string, options map[string]apiextensionsv1.JSON) error {
	_, err := c.ResolveConfig(model, options)
	return err
}

// ResolveConfig validates Copilot configuration and applies the explicit
// fallback-to-default policy when a requested model is unavailable.
func (c *CopilotAdapter) ResolveConfig(model string, options map[string]apiextensionsv1.JSON) (ConfigResolution, error) {
	return c.resolveConfig(context.Background(), model, options)
}

func (c *CopilotAdapter) resolveConfig(ctx context.Context, model string, options map[string]apiextensionsv1.JSON) (ConfigResolution, error) {
	effectiveOptions, fallback, err := copilotFallbackOption(options)
	if err != nil {
		return ConfigResolution{}, err
	}
	if model == "" && fallback {
		return ConfigResolution{}, fmt.Errorf("harness option %q requires an explicit model", fallbackToDefaultOption)
	}
	var capabilities copilotModelCapabilities
	if model != "" {
		availableModels, err := c.discoverModels(ctx)
		if err != nil {
			// Discovery failing is "cannot determine availability", not "the
			// model is invalid". Config admission runs wherever the daemon or
			// `goobers lint` runs — including CI runners and developer machines
			// with no Copilot CLI installed or no authenticated session — so
			// treating an unreachable harness as a validation error makes a
			// config's validity depend on what happens to be installed on the
			// validating machine. Accept the requested model, record that it
			// went unchecked, and let the run itself fail later if the model is
			// genuinely wrong.
			//
			// Capability-dependent option checks are skipped rather than run
			// against zero-value capabilities, which would spuriously reject
			// long_context and every reasoningEffort value. Shape validation
			// (unknown options, non-string values, options that require an
			// explicit model) is capability-independent and still applies.
			if _, normalizeErr := normalizeResolvedCopilotConfig(model, effectiveOptions); normalizeErr != nil {
				return ConfigResolution{}, normalizeErr
			}
			return ConfigResolution{
				Model:          model,
				HarnessOptions: effectiveOptions,
				Warnings: []ConfigWarning{{
					Kind: ConfigWarningModelUnverified,
					Message: fmt.Sprintf(
						"could not reach the Copilot harness to verify model %q: %v; accepting it unverified",
						model, err),
				}},
			}, nil
		}
		var ok bool
		capabilities, ok = availableModels[model]
		if !ok {
			validModels := make([]string, 0, len(availableModels))
			for name := range availableModels {
				validModels = append(validModels, name)
			}
			sort.Strings(validModels)
			if !fallback {
				return ConfigResolution{}, fmt.Errorf("unknown model %q; valid models: %s", model, strings.Join(validModels, ", "))
			}
			if _, err := normalizeCopilotConfig("", effectiveOptions, copilotModelCapabilities{}); err != nil {
				return ConfigResolution{}, fmt.Errorf("fall back model %q to the harness default: %w", model, err)
			}
			return ConfigResolution{
				HarnessOptions: effectiveOptions,
				Warnings: []ConfigWarning{{
					Kind:    ConfigWarningModelFallback,
					Message: fmt.Sprintf("requested model %q is unavailable; using the harness default", model),
				}},
			}, nil
		}
	}
	if _, err := normalizeCopilotConfig(model, effectiveOptions, capabilities); err != nil {
		return ConfigResolution{}, err
	}
	return ConfigResolution{
		Model:          model,
		HarnessOptions: effectiveOptions,
	}, nil
}

func (c *CopilotAdapter) discoverModels(ctx context.Context) (map[string]copilotModelCapabilities, error) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if c.availableModels != nil {
		return c.availableModels, nil
	}
	if c.DeferDiscovery {
		return nil, errModelDiscoveryDeferred
	}
	if c.modelsErr != nil {
		return nil, c.modelsErr
	}
	command := resolveStdioHarnessCommand(c.Command)
	if len(command) == 0 {
		return nil, fmt.Errorf("no command configured")
	}
	lister := c.ModelLister
	if lister == nil {
		lister = sdkCopilotModelLister{}
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, copilotModelDiscoveryTimeout)
	defer cancel()
	models, err := lister.ListModels(discoveryCtx, command, baseEnv(c.ExtraEnvAllowlist))
	if err != nil {
		c.modelsErr = err
		return nil, err
	}
	if len(models) == 0 {
		c.modelsErr = fmt.Errorf("authenticated Copilot runtime returned no available models")
		return nil, c.modelsErr
	}
	available := make(map[string]copilotModelCapabilities, len(models)+1)
	autoDisabled := false
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		if model.PolicyState == copilotModelPolicyDisabled {
			if model.ID == "auto" {
				autoDisabled = true
			}
			continue
		}
		efforts := make(map[string]struct{}, len(model.SupportedReasoningEfforts))
		for _, effort := range model.SupportedReasoningEfforts {
			efforts[effort] = struct{}{}
		}
		available[model.ID] = copilotModelCapabilities{
			longContext:     copilotLongContextModels[model.ID],
			reasoningEffort: efforts,
		}
	}
	if !autoDisabled {
		available["auto"] = copilotModelCapabilities{}
	}
	if len(available) == 0 {
		return nil, fmt.Errorf("authenticated Copilot runtime returned no available models")
	}
	c.availableModels = available
	return c.availableModels, nil
}

func copilotFallbackOption(options map[string]apiextensionsv1.JSON) (map[string]apiextensionsv1.JSON, bool, error) {
	if len(options) == 0 {
		return nil, false, nil
	}
	effective := make(map[string]apiextensionsv1.JSON, len(options))
	fallback := false
	for name, value := range options {
		if name != fallbackToDefaultOption {
			effective[name] = apiextensionsv1.JSON{Raw: append([]byte(nil), value.Raw...)}
			continue
		}
		var configured *bool
		if err := json.Unmarshal(value.Raw, &configured); err != nil {
			return nil, false, fmt.Errorf("harness option %q must be a boolean: %w", name, err)
		}
		if configured == nil {
			return nil, false, fmt.Errorf("harness option %q must be a boolean", name)
		}
		fallback = *configured
	}
	if len(effective) == 0 {
		effective = nil
	}
	return effective, fallback, nil
}

func normalizeCopilotConfig(model string, options map[string]apiextensionsv1.JSON, capabilities copilotModelCapabilities) (map[string]string, error) {
	normalized, err := normalizeResolvedCopilotConfig(model, options)
	if err != nil {
		return nil, err
	}
	if normalized["context"] == "long_context" && !capabilities.longContext {
		return nil, fmt.Errorf("context value %q is not supported by model %q", normalized["context"], model)
	}
	if value, ok := normalized["reasoningEffort"]; ok {
		if _, supported := capabilities.reasoningEffort[value]; !supported {
			return nil, fmt.Errorf("reasoningEffort value %q is not supported by model %q", value, model)
		}
	}
	return normalized, nil
}

func normalizeResolvedCopilotConfig(model string, options map[string]apiextensionsv1.JSON) (map[string]string, error) {
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
			}
		case "reasoningEffort":
			if model == "" {
				return nil, fmt.Errorf("reasoningEffort requires an explicit model")
			}
		}
		normalized[name] = value
	}
	return normalized, nil
}

// Preflight verifies the Copilot CLI binary is on PATH and responds to a
// version check, returning its reported version on success.
func (c *CopilotAdapter) Preflight(ctx context.Context) (PreflightInfo, error) {
	if len(c.Command) == 0 {
		return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: no command configured")
	}
	bin := c.Command[0]
	if _, err := exec.LookPath(bin); err != nil {
		return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: %q not found on PATH — install the GitHub Copilot CLI "+
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
	versionProbe := fmt.Sprintf("harness: copilot-cli: %q %v", bin, args)
	res, err := c.runner().Run(ctx, ProcessRequest{
		Command:            append([]string{bin}, args...),
		Env:                baseEnv(c.ExtraEnvAllowlist),
		MaxTranscriptBytes: maxPreflightDiagnosticBytes,
	})
	if err != nil || res.ExitCode != 0 {
		return PreflightInfo{}, preflightProbeError(versionProbe, res, err, "check that the CLI is installed and authenticated")
	}
	version := firstOutputLine(res.Transcript)
	if version == "" {
		return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: %q %v returned no version", bin, args)
	}
	// A signed-out CLI passes --version but can't do agentic work, so probe
	// authentication too when configured (GBO-011, #238) — catching it here at
	// startup rather than as a burned mid-run agentic attempt.
	if len(c.AuthCheckArgs) > 0 {
		command := resolveHarnessCommand(c.Command)
		// Preflight has no RunRequest, so it cannot resolve the agent:model
		// credential the way credentialEnv does at run time — the sign-in probe
		// would only ever reflect an ambient CLI login and would fail a valid
		// headless-PAT setup (COPILOT_GITHUB_TOKEN provided via env), then burn
		// the whole run at the first agentic stage. When a copilot model token is
		// present in the ambient environment, carry it into the probe so the
		// check reflects the same auth the run will use.
		authEnv := baseEnv(c.ExtraEnvAllowlist)
		if tok := ambientCopilotToken(); tok != "" {
			authEnv = overrideEnv(authEnv, "COPILOT_GITHUB_TOKEN", tok)
		}
		authProbe := fmt.Sprintf("harness: copilot-cli: %q %v (sign-in check)", bin, c.AuthCheckArgs)
		res, err := c.runner().Run(ctx, ProcessRequest{
			Command:            append(command, c.AuthCheckArgs...),
			Env:                authEnv,
			MaxTranscriptBytes: maxPreflightDiagnosticBytes,
		})
		if err != nil || res.ExitCode != 0 {
			return PreflightInfo{}, preflightProbeError(authProbe, res, err, "if this is an authentication failure, run the Copilot CLI and sign in")
		}
	}
	return PreflightInfo{Version: version}, nil
}

// ambientCopilotToken returns a Copilot model token found in the ambient
// process environment, if any, so the sign-in preflight can reflect a headless
// PAT setup that provides the token by env rather than an interactive CLI
// login. COPILOT_GITHUB_TOKEN is the CLI's own variable; GH_TOKEN/GITHUB_TOKEN
// are accepted as the conventional fallbacks the Copilot CLI also honors.
func ambientCopilotToken() string {
	for _, name := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

func firstOutputLine(output []byte) string {
	for line := range strings.SplitSeq(string(output), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func copilotAvailableTools(req RunRequest) []string {
	var tools []string
	seen := make(map[string]struct{})
	appendTool := func(tool string) {
		if _, ok := seen[tool]; ok {
			return
		}
		seen[tool] = struct{}{}
		tools = append(tools, tool)
	}
	for _, declaredTool := range req.Tools {
		for _, server := range req.MCPServers {
			appendTool(server.Name + "-" + declaredTool)
		}
		for _, tool := range expandToolGroup(declaredTool, copilotToolGroups) {
			appendTool(tool)
		}
	}
	return tools
}

func validateCopilotTools(tools []string) error {
	return validateToolAllowlist("copilot-cli", tools)
}

func copilotDeclaresTool(declared []string, target string) bool {
	for _, tool := range declared {
		if strings.EqualFold(tool, target) {
			return true
		}
	}
	return false
}

func copilotConstraintConflict(args []string) string {
	for _, arg := range args {
		if arg == "--available-tools" || strings.HasPrefix(arg, "--available-tools=") ||
			arg == "--output-format" || strings.HasPrefix(arg, "--output-format=") {
			return arg
		}
	}
	return ""
}

func (c *CopilotAdapter) runner() ProcessRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecProcessRunner{}
}

// Run renders the prompt, runs the CLI non-interactively in req.Workspace with
// capability-scoped credentials in its environment, prefers converted native
// session events over the subprocess transcript when available, and captures
// the completion through either the default file contract or the final response
// used by tool-constrained sessions.
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
	// Auto-wire goobers-io (#2406) before anything below reads req.Tools or
	// req.MCPServers: completionInResponse, the rendered prompt, and the MCP
	// credential/prep block all need to see the goobers-io server and tools
	// as already present, not added after the fact. Every valid invocation
	// receives run identity access, even without artifact or context inputs.
	req = withAutoGoobersIO(req, c.SelfBin)
	resolution := ConfigResolution{
		Model:          req.Model,
		HarnessOptions: cloneHarnessOptions(req.HarnessOptions),
	}
	if !req.HarnessConfigResolved {
		var err error
		resolution, err = c.resolveConfig(ctx, req.Model, req.HarnessOptions)
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: copilot-cli: invalid configuration: %w", err)
		}
	}
	harnessOptions, err := normalizeResolvedCopilotConfig(resolution.Model, resolution.HarnessOptions)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: invalid resolved configuration: %w", err)
	}
	if len(req.MCPServers) > 0 {
		if err := c.requireMCPModelCredential(ctx, req); err != nil {
			return Outcome{}, err
		}
	}
	if err := validateCopilotTools(req.Tools); err != nil {
		return Outcome{}, err
	}

	completionInResponse := len(req.Tools) > 0
	prompt := renderPrompt(req)
	if completionInResponse {
		prompt = renderResponseCompletionPrompt(req)
	}
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
	baseCommand := resolveHarnessCommand(c.Command)
	if completionInResponse {
		configuredArgs := append(append([]string(nil), baseCommand[1:]...), extra...)
		if conflict := copilotConstraintConflict(configuredArgs); conflict != "" {
			return Outcome{}, fmt.Errorf("harness: copilot-cli: tool-constrained run conflicts with configured argument %q", conflict)
		}
	}
	argv := append(baseCommand, flag, prompt)
	promptArg := len(baseCommand) + 1
	if resolution.Model != "" {
		argv = append(argv, "--model", resolution.Model)
	}
	if value, ok := harnessOptions["context"]; ok {
		argv = append(argv, "--context", value)
	}
	if value, ok := harnessOptions["reasoningEffort"]; ok {
		argv = append(argv, "--reasoning-effort", value)
	}
	argv = append(argv, extra...)
	if completionInResponse {
		if copilotDeclaresTool(req.Tools, "github") {
			argv = append(argv, "--add-github-mcp-toolset=issues")
		}
		argv = append(argv,
			"--available-tools="+strings.Join(copilotAvailableTools(req), ","),
			"--silent",
			"--output-format=text",
		)
	}
	// goobers-io (#2406) is delivered independently of req.MCPServers/
	// prepareCopilotMCP below — see copilot_mcp_io.go's goobersIORuntimeSubdir
	// doc comment for why: it's a harness-owned server with no credentials of
	// its own, and routing it through the same pipeline as genuinely external
	// servers broke documented stored-CLI-login auth and rejected otherwise-
	// valid credentialed-MCP stages (both confirmed live).
	mcpArg, err := goobersIOAdditionalMCPConfigArg(req, c.SelfBin)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: %w", err)
	}
	if mcpArg != "" {
		argv = append(argv, "--additional-mcp-config", "@"+mcpArg)
	}

	env, err := c.credentialEnv(ctx, req)
	if err != nil {
		return Outcome{}, err
	}
	// Enforced isolation posture (S3/#166): route the CLI's own runtime state
	// into the workspace so the sandbox policy needs no writable root beyond
	// the worktree (plus its narrowed linked git directories) — the exact recipe the
	// sandbox package's live Copilot probe codified (ADR-0001). The overrides
	// happen before the session-id block below so the native transcript path
	// derives from the confined COPILOT_HOME.
	var confinement *copilotConfinement
	if req.Sandbox != nil {
		confinement, err = prepareCopilotConfinement(req.Workspace)
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: copilot-cli: sandbox: %w", err)
		}
		env = overrideEnv(env, "COPILOT_HOME", confinement.copilotHome)
		env = overrideEnv(env, "TMPDIR", confinement.tempDir)
		argv = append(argv, "--log-dir", confinement.logDir)
	}
	if len(req.MCPServers) > 0 {
		env, err = prepareCopilotMCP(ctx, req, env)
		if err != nil {
			return Outcome{}, err
		}
		if !copilotDeclaresTool(req.Tools, "github") {
			argv = append(argv, "--disable-builtin-mcps")
		}
	}
	nativeTranscriptPath := ""
	if !copilotCommandSelectsSession(argv) {
		captureID, err := newHarnessSessionID()
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: copilot-cli: create transcript capture id: %w", err)
		}
		argv = append(argv, "--session-id", captureID)
		// Pin the log to this run without replacing the home that also holds
		// the user's Copilot configuration.
		if copilotHome, ok := copilotConfigHome(env); ok {
			nativeTranscriptPath = copilotSessionLogPath(copilotHome, captureID)
		}
	}

	if req.Sandbox != nil {
		// Wrap last, once argv is final (session id included), so the whole
		// invocation runs inside the sandbox. promptArg shifts by the wrapper
		// prefix so the contract-recovery turn below still swaps the prompt.
		wrapped, shift, err := confineArgv(req.Sandbox, argv, req.Workspace, confinement.writableRoots)
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: copilot-cli: sandbox: %w", err)
		}
		argv = wrapped
		promptArg += shift
	}

	// #2962: record the CLI version and the effective tool/permission
	// arguments before the session starts. When a run later reports a tool
	// refusal, this is what distinguishes "the goober was never granted the
	// tool" from "the CLI changed how it grants tools" — previously
	// unanswerable after the fact, because the invocation was never kept.
	// Only permission-relevant flags are recorded; the prompt and environment
	// are deliberately excluded (they carry task content and credentials).
	if err := writeCopilotInvocationDiagnostics(req, argv); err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: %w", err)
	}

	runner := c.runner()
	started := time.Now()
	var responseCapture *syncBuffer
	var stdoutCapture io.Writer
	if completionInResponse {
		responseCapture = newTranscriptBuffer(req.MaxTranscriptBytes)
		stdoutCapture = responseCapture
	}
	result, runErr := runner.Run(ctx, ProcessRequest{
		Command:            argv,
		Dir:                req.Workspace,
		Env:                env,
		Timeout:            req.Timeout,
		MaxTranscriptBytes: req.MaxTranscriptBytes,
		StdoutCapture:      stdoutCapture,
	})
	var payload []byte
	var completionErr error
	if runErr == nil {
		payload, completionErr = readCopilotCompletion(req, responseCapture, completionInResponse)
		if errors.Is(completionErr, ErrNoCompletion) {
			// A clean Copilot exit can still omit its completion contract. Give
			// the same session one contract-only turn without extending its budget.
			totalTimeout := req.Timeout
			if totalTimeout <= 0 {
				totalTimeout = DefaultTimeout
			}
			remaining := totalTimeout - time.Since(started)
			if remaining <= 0 {
				runErr = fmt.Errorf("%w after %s: %s", ErrTimeout, totalTimeout, argv[0])
				completionErr = nil
			} else {
				recoveryArgv := append([]string(nil), argv...)
				recoveryPrompt := renderCompletionRecoveryPrompt(req)
				var recoveryCapture *syncBuffer
				var recoveryStdout io.Writer
				if completionInResponse {
					recoveryPrompt = renderResponseCompletionRecoveryPrompt(req)
					recoveryCapture = newTranscriptBuffer(req.MaxTranscriptBytes)
					recoveryStdout = recoveryCapture
				}
				recoveryArgv[promptArg] = recoveryPrompt
				recovery, err := runner.Run(ctx, ProcessRequest{
					Command:            recoveryArgv,
					Dir:                req.Workspace,
					Env:                env,
					Timeout:            remaining,
					MaxTranscriptBytes: req.MaxTranscriptBytes,
					StdoutCapture:      recoveryStdout,
				})
				result = mergeProcessResults(result, recovery, req.MaxTranscriptBytes)
				if err != nil {
					runErr = err
					completionErr = nil
				} else {
					payload, completionErr = readCopilotCompletion(req, recoveryCapture, completionInResponse)
				}
			}
		}
	}
	out := Outcome{
		Transcript:             result.Transcript,
		RenderedPrompt:         []byte(prompt),
		TranscriptTruncated:    result.TranscriptTruncated,
		TranscriptDroppedBytes: result.TranscriptDroppedBytes,
		Stderr:                 result.Stderr,
	}
	receipts, receiptsCollected, receiptsErr := collectGoobersIOReceipts(req, c.SelfBin)
	out.InputInspectionReceipts = receipts
	out.InputInspectionReceiptsCollected = receiptsCollected
	if receiptsErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("read goobers-io input inspection receipts: %w", receiptsErr))
	}
	if nativeTranscriptPath != "" {
		if native, ok := readCopilotSessionTranscript(nativeTranscriptPath, req.MaxTranscriptBytes); ok {
			out.Metrics = native.metrics
			out.ModelUsage = native.modelUsage
			if len(native.data) > 0 {
				out.Transcript = native.data
				out.TranscriptSchema = telemetry.GenAIEventSchema
				out.TranscriptTruncated = native.truncated
				out.TranscriptDroppedBytes = native.droppedBytes
			}
		}
	}
	if runErr != nil {
		return out, runErr
	}
	if completionErr != nil {
		return out, completionErr
	}
	out.Payload = payload
	return out, nil
}

func readCopilotCompletion(req RunRequest, capture *syncBuffer, completionInResponse bool) ([]byte, error) {
	if !completionInResponse {
		return readCompletion(req.Workspace, req.CompletionPath)
	}
	payload, responseErr := readCopilotResponseCompletion(req.Mode, capture)
	if responseErr == nil {
		return payload, nil
	}
	payload, fileErr := readCompletion(req.Workspace, req.CompletionPath)
	switch {
	case fileErr == nil:
		if err := validateCopilotCompletion(req.Mode, payload); err != nil {
			return nil, fmt.Errorf("%w: Copilot completion file failed validation: %w", ErrNoCompletion, err)
		}
		return payload, nil
	case !errors.Is(fileErr, ErrNoCompletion):
		return nil, fileErr
	default:
		return nil, responseErr
	}
}

func readCopilotResponseCompletion(mode Mode, capture *syncBuffer) ([]byte, error) {
	if capture == nil {
		return nil, fmt.Errorf("%w: Copilot final response was not captured", ErrNoCompletion)
	}
	if capture.Truncated() {
		return nil, fmt.Errorf("%w: Copilot final response exceeded the %d-byte capture limit",
			ErrNoCompletion, len(capture.retainedBytes())+int(capture.Dropped()))
	}
	payload := bytes.TrimSpace(capture.retainedBytes())
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: Copilot returned an empty final response", ErrNoCompletion)
	}
	payload = extractCompletionJSON(payload)
	if !json.Valid(payload) {
		return nil, fmt.Errorf("%w: Copilot final response is not valid JSON", ErrNoCompletion)
	}
	if err := validateCopilotCompletion(mode, payload); err != nil {
		return nil, fmt.Errorf("%w: Copilot final response failed validation: %w", ErrNoCompletion, err)
	}
	return payload, nil
}

// extractCompletionJSON tolerates the two most common ways a tool-constrained
// model deviates from the "entire final response is bare JSON" contract despite
// the explicit instruction: wrapping the JSON in a Markdown code fence, and
// surrounding it with a short prose preamble or trailer. It returns the isolated
// JSON payload when it can confidently find one, otherwise the trimmed input
// unchanged so the caller's json.Valid check still reports the original failure.
func extractCompletionJSON(payload []byte) []byte {
	trimmed := bytes.TrimSpace(payload)
	if json.Valid(trimmed) {
		return trimmed
	}
	if unfenced, ok := stripCodeFence(trimmed); ok {
		unfenced = bytes.TrimSpace(unfenced)
		if json.Valid(unfenced) {
			return unfenced
		}
		trimmed = unfenced
	}
	if inner, ok := firstJSONValue(trimmed); ok {
		return inner
	}
	return trimmed
}

// stripCodeFence removes a single leading ``` (optionally with a one-word
// language tag such as ```json) and its matching trailing ``` when the payload
// is wrapped in one Markdown fenced block. It returns (unwrapped, true) only
// when both the opening and closing fences are present.
func stripCodeFence(payload []byte) ([]byte, bool) {
	s := bytes.TrimSpace(payload)
	if !bytes.HasPrefix(s, []byte("```")) {
		return payload, false
	}
	nl := bytes.IndexByte(s, '\n')
	if nl < 0 {
		return payload, false
	}
	// The opening fence line is ``` plus an optional single-token language tag
	// (e.g. "json"). Reject anything with interior whitespace so a stray "```"
	// that opens a prose sentence is not mistaken for a real fence.
	fenceInfo := bytes.TrimSpace(s[3:nl])
	if bytes.ContainsAny(fenceInfo, " \t") {
		return payload, false
	}
	body := s[nl+1:]
	end := bytes.LastIndex(body, []byte("```"))
	if end < 0 {
		return payload, false
	}
	return body[:end], true
}

// firstJSONValue scans for the first balanced JSON object or array in payload,
// honoring string literals and escapes so braces or brackets inside strings do
// not corrupt the depth count. It returns (value, true) only when the extracted
// span parses as valid JSON.
func firstJSONValue(payload []byte) ([]byte, bool) {
	start := bytes.IndexAny(payload, "{[")
	if start < 0 {
		return nil, false
	}
	opener := payload[start]
	closer := byte('}')
	if opener == '[' {
		closer = ']'
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(payload); i++ {
		ch := payload[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case opener:
			depth++
		case closer:
			depth--
			if depth == 0 {
				candidate := payload[start : i+1]
				if json.Valid(candidate) {
					return candidate, true
				}
				return nil, false
			}
		}
	}
	return nil, false
}

func validateCopilotCompletion(mode Mode, payload []byte) error {
	validator, err := validate.New()
	if err != nil {
		return fmt.Errorf("build completion validator: %w", err)
	}
	kind := "result"
	if mode == ModeReview {
		kind = "verdict"
	}
	if err := validator.ValidateEnvelope(kind, payload); err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	return nil
}

func mergeProcessResults(first, second ProcessResult, limit int64) ProcessResult {
	firstTranscript, secondTranscript, dropped := retainedProcessTranscripts(first, second, limit)
	firstStderr, secondStderr, stderrDropped := retainedProcessOutput(
		processOutput{data: first.Stderr, dropped: first.StderrDroppedBytes},
		processOutput{data: second.Stderr, dropped: second.StderrDroppedBytes},
		limit,
	)

	transcript := append([]byte(nil), firstTranscript...)
	if len(firstTranscript) > 0 && len(secondTranscript) > 0 {
		transcript = append(transcript, '\n')
	}
	transcript = append(transcript, secondTranscript...)
	if dropped > 0 {
		transcript = append(transcript, transcriptTruncationMarker(dropped)...)
	}
	stderr := append([]byte(nil), firstStderr...)
	if len(firstStderr) > 0 && len(secondStderr) > 0 {
		stderr = append(stderr, '\n')
	}
	stderr = append(stderr, secondStderr...)
	if stderrDropped > 0 {
		stderr = append(stderr, transcriptTruncationMarker(stderrDropped)...)
	}
	return ProcessResult{
		Transcript:             transcript,
		ExitCode:               second.ExitCode,
		TranscriptTruncated:    first.TranscriptTruncated || second.TranscriptTruncated || dropped > 0,
		TranscriptDroppedBytes: dropped,
		Stderr:                 stderr,
		StderrTruncated:        first.StderrTruncated || second.StderrTruncated || stderrDropped > 0,
		StderrDroppedBytes:     stderrDropped,
	}
}

func retainedProcessTranscripts(first, second ProcessResult, limit int64) ([]byte, []byte, int64) {
	return retainedProcessOutput(
		processOutput{data: first.Transcript, dropped: first.TranscriptDroppedBytes},
		processOutput{data: second.Transcript, dropped: second.TranscriptDroppedBytes},
		limit,
	)
}

type processOutput struct {
	data    []byte
	dropped int64
}

func retainedProcessOutput(first, second processOutput, limit int64) ([]byte, []byte, int64) {
	if limit <= 0 {
		limit = DefaultMaxTranscriptBytes
	}

	firstBytes := processOutputBytes(first)
	secondBytes := processOutputBytes(second)

	// The recovery turn is the most useful diagnostic when the first turn
	// omitted its contract, so retain it first and use the remaining allowance
	// for the initial turn.
	secondRetained := min(int64(len(secondBytes)), limit)
	remaining := limit - secondRetained
	var firstRetained int64
	if secondRetained == 0 {
		firstRetained = min(int64(len(firstBytes)), remaining)
	} else if len(firstBytes) > 0 && remaining > 1 {
		firstRetained = min(int64(len(firstBytes)), remaining-1)
	}
	dropped := first.dropped + second.dropped +
		int64(len(firstBytes)) - firstRetained +
		int64(len(secondBytes)) - secondRetained

	return firstBytes[:firstRetained], secondBytes[:secondRetained], dropped
}

func processOutputBytes(output processOutput) []byte {
	if output.dropped <= 0 {
		return output.data
	}
	return bytes.TrimSuffix(output.data, transcriptTruncationMarker(output.dropped))
}

func processTranscriptBytes(result ProcessResult) []byte {
	return processOutputBytes(processOutput{data: result.Transcript, dropped: result.TranscriptDroppedBytes})
}

// credentialEnv builds the subprocess environment: baseEnv() (PATH/HOME/
// TMPDIR — never a secret store, never the full os.Environ()), the stage
// telemetry directory, routed repository and instance context, the running
// goobers executable path, and exactly the capability tokens this adapter is
// configured to inject and that were actually declared for this invocation.
// A configured capability that fails to resolve is a hard stop — the harness
// never runs half-credentialed.
func (c *CopilotAdapter) credentialEnv(ctx context.Context, req RunRequest) ([]string, error) {
	return buildCredentialEnv(ctx, credentialEnvConfig{
		adapterName:                    c.Name(),
		envCapabilities:                c.EnvCapabilities,
		optionalCredentialCapabilities: c.OptionalCredentialCapabilities,
		extraEnvAllowlist:              c.ExtraEnvAllowlist,
		instanceRoot:                   c.InstanceRoot,
		selfBin:                        c.SelfBin,
	}, req)
}

func (c *CopilotAdapter) requireMCPModelCredential(ctx context.Context, req RunRequest) error {
	modelCapability := string(capability.AgentModel)
	if c.EnvCapabilities[modelCapability] == "" {
		return fmt.Errorf("harness: copilot-cli: external MCP servers require an environment binding for %s", modelCapability)
	}
	if req.Credentials == nil {
		return fmt.Errorf("harness: copilot-cli: external MCP servers require a materialized %s credential", modelCapability)
	}
	if _, err := req.Credentials.Token(ctx, modelCapability); err != nil {
		return fmt.Errorf("harness: copilot-cli: external MCP servers require a materialized %s credential: %w", modelCapability, err)
	}
	return nil
}
