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
	"github.com/goobers/goobers/internal/ephemeraltmp"
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
	// ModelCredential resolves the instance's configured agent:model tokenRef
	// (file/keychain/store — env is already covered by ambientCopilotToken)
	// for the two config-time paths that have no RunRequest and so cannot go
	// through the normal credentialEnv/req.Credentials resolution: the
	// Preflight sign-in probe and admission-time model discovery.
	//
	// Non-nil means the instance HAS an agent:model grant
	// (cmd/goobers.agentModelCredentialResolver returns nil when it has none),
	// and it is then the authority — see copilotModelToken for why it outranks
	// an ambient env var. Nil is valid: both paths then reflect only ambient
	// env or the CLI's own cached login, exactly as before this field existed.
	// An error it returns fails the caller outright rather than being
	// swallowed, since a misconfigured tokenRef must surface as an actionable
	// error, not silently fall back to a stale cached login.
	ModelCredential func(ctx context.Context) (string, error)
	// InstanceRoot is exposed to the agentic subprocess so a goobers CLI command
	// it invokes can resolve instance configuration outside the stage worktree.
	InstanceRoot string
	// SelfBin is the running daemon's executable path. It is exposed to the
	// agentic subprocess so tools can invoke that exact goobers binary.
	SelfBin string
	// EphemeralTmp binds the `tmp:ephemeral` restriction on the SELF runner
	// (docs/design/goobernetes-restrictions.md §2.4) for this adapter's
	// subprocess: an attempt-private temp directory carved out of the daemon's
	// temp root, with the temp-nested build caches (GOCACHE, GOMODCACHE, ...)
	// re-rooted into it, destroyed when the attempt returns.
	//
	// It is a RUNNER property, set by wiring from the resolved inventory's
	// self entry, so every agentic stage placed on self runs under it whether
	// or not it asked (goobernetes-restrictions.md §5). Off by default: an
	// instance that declares no runners builds a byte-identical subprocess
	// environment to before this field existed.
	//
	// The adapter's own in-workspace runtime confinement still wins for
	// TMPDIR where it applies (the enforced-sandbox path below), because that
	// area is already attempt-private, rides workspace cleanup, and is inside
	// the sandbox's writable roots. What this adds there is the reclaim of the
	// build caches the agent's own toolchain invocations write.
	//
	// Failure to establish the private area fails the stage CLOSED.
	EphemeralTmp bool
	// EphemeralTmpRoot overrides the temp root the per-attempt directory is
	// carved out of. Empty means the daemon's own temp root (os.TempDir()) —
	// the same medium the stage's temp would otherwise have used.
	EphemeralTmpRoot string
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

// AvailableTools implements ToolSurfaceReporter: it reports the concrete
// model-facing tool IDs a declared allowlist expands to, so the capability
// preflight (#2197) checks the surface a session actually receives rather
// than a re-derived copy of the expansion.
func (c *CopilotAdapter) AvailableTools(declared []string) []string {
	return copilotAvailableTools(RunRequest{Tools: declared})
}

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
	// #4292: this call used to build its env from baseEnv() alone, with no
	// credential of any kind, leaving config admission (workflow load) unable
	// to see a file/keychain/store-sourced agent:model credential — which
	// forced every deployment to also deliver the token as a raw ambient env
	// var just so discovery could authenticate. It now takes the same token
	// the auth-check probe below does, on the same resolver-first order (see
	// copilotModelToken).
	discoveryEnv := baseEnv(c.ExtraEnvAllowlist)
	tok, credErr := c.copilotModelToken(discoveryCtx)
	if credErr != nil {
		c.modelsErr = fmt.Errorf("resolve agent:model credential for model discovery: %w", credErr)
		return nil, c.modelsErr
	}
	if tok != "" {
		discoveryEnv = overrideEnv(discoveryEnv, "COPILOT_GITHUB_TOKEN", tok)
	}
	models, err := lister.ListModels(discoveryCtx, command, discoveryEnv)
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
		// credential the way credentialEnv does at run time — left to itself the
		// sign-in probe would reflect only whatever the CLI cached from its own
		// prior interactive login, a different and possibly wrong account
		// (#3341), and would fail a valid headless setup outright. Carry the
		// instance's configured credential into the probe instead, falling back
		// to an ambient env var when the instance declares no grant; see
		// copilotModelToken for why that order and not the reverse (#4292).
		authEnv := baseEnv(c.ExtraEnvAllowlist)
		tok, err := c.copilotModelToken(ctx)
		if err != nil {
			return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: resolve agent:model credential: %w", err)
		}
		if tok != "" {
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

// copilotModelToken resolves the token the config-time Copilot probes (the
// sign-in preflight and admission-time model discovery) should authenticate
// with: the instance's configured agent:model credential when it has one, and
// otherwise whatever the ambient process environment carries.
//
// The order is resolver-first, and that is the whole point (#4292). Stage
// execution never reads the ambient environment for this: baseEnv is a
// default-deny allowlist that does not carry COPILOT_GITHUB_TOKEN, GH_TOKEN or
// GITHUB_TOKEN at all, so the run authenticates with the resolver's credential
// and nothing else. A probe that preferred an ambient var would therefore
// validate an account the run will not use — preflight green, run wrong or
// failed — which is the same wrong-account confusion (#3341) that put a
// resolver on this path to begin with, just entered from the other side.
//
// It also ends the double delivery the issue reports: with the resolver
// authoritative, a Kubernetes deployment can drop the duplicate plain env var
// from its pod spec and keep the one credential the resolver can scope, rotate
// and audit. The ambient read stays as the documented fallback for a local or
// laptop instance that declares no agent:model grant and exports GH_TOKEN in a
// shell profile — that instance is unchanged.
//
// A resolution error is returned, never swallowed: a misconfigured tokenRef
// (bad path, unreadable file, empty secret) must fail loudly rather than
// degrade into whatever login the CLI happens to have cached.
func (c *CopilotAdapter) copilotModelToken(ctx context.Context) (string, error) {
	if c.ModelCredential != nil {
		return c.ModelCredential(ctx)
	}
	return ambientCopilotToken(), nil
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
func (c *CopilotAdapter) Run(ctx context.Context, req RunRequest) (out Outcome, runErr error) {
	if err := validateStandardExecution(req); err != nil {
		return Outcome{}, err
	}
	if len(c.Command) == 0 {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: no command configured")
	}
	if req.Workspace == "" {
		// exec.Cmd treats Dir == "" as "run in the daemon's own working
		// directory" — a silent, surprising fallback (#122) rather than the
		// fail-closed misconfiguration error an unset workspace should be.
		return Outcome{}, fmt.Errorf("harness: copilot-cli: RunRequest.Workspace is empty")
	}
	// The self-runner tmp:ephemeral binding for this attempt. Established
	// before anything below builds an environment, reclaimed on every exit
	// path — including the error returns between here and the subprocess.
	ephemeralTmp, err := establishEphemeralTmp(c.Name(), c.EphemeralTmp, c.EphemeralTmpRoot)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = ephemeralTmp.Reclaim() }()
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
	if err := os.WriteFile(debugPath, []byte(prompt), 0o600); err != nil {
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

	env, err := c.credentialEnv(ctx, ephemeralTmp, req)
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

	agentTelemetry, err := beginAdapterAgentTelemetry(
		req, "copilot", req.Model, resolution.Model,
		requestedHarnessOption(req, "reasoningEffort"), harnessOptions["reasoningEffort"],
	)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: start agent telemetry: %w", err)
	}
	defer agentTelemetry.finish(&out, &runErr)

	runner := c.runner()
	started := time.Now()
	var responseCapture *syncBuffer
	var stdoutCapture io.Writer
	if completionInResponse {
		responseCapture = newTranscriptBuffer(req.MaxTranscriptBytes)
		stdoutCapture = responseCapture
	}
	result, processErr := runner.Run(ctx, ProcessRequest{
		Command:            argv,
		Dir:                req.Workspace,
		Env:                env,
		Timeout:            req.Timeout,
		MaxTranscriptBytes: req.MaxTranscriptBytes,
		StdoutCapture:      stdoutCapture,
		// #4179: the session this observes is the one that burned a whole
		// 5400s budget on a stalled `go mod download` while its journal held
		// a single lifecycle event.
		Activity: agentTelemetry.activityObserver(),
	})
	runErr = processErr
	var payload []byte
	var completionErr error
	if processErr == nil {
		payload, completionErr = readCopilotCompletion(req, responseCapture, completionInResponse)
		if errors.Is(completionErr, ErrNoCompletion) && nativeTranscriptPath != "" {
			// Copilot does not reliably echo its final message to stdout under
			// --silent --output-format=text with MCP tools attached: the answer
			// lands in the session log while the stdout capture stays empty, so
			// the read above reports "final response is not valid JSON" for a
			// completion the model produced correctly. Recover it from the log
			// before spending the contract-recovery turn (which re-runs the whole
			// session and hits the same stdout gap, failing the stage twice and
			// stranding committed work on the branch).
			if recovered, ok := readCopilotCompletionFromSession(req.Mode, nativeTranscriptPath, req.MaxTranscriptBytes); ok {
				payload, completionErr = recovered, nil
			}
		}
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
					// The recovery turn runs on what is LEFT of the budget,
					// so a stall here is if anything more urgent to see than
					// one in the main session (#4179).
					Activity: agentTelemetry.activityObserver(),
				})
				result = mergeProcessResults(result, recovery, req.MaxTranscriptBytes)
				if err != nil {
					runErr = err
					completionErr = nil
				} else {
					payload, completionErr = readCopilotCompletion(req, recoveryCapture, completionInResponse)
					if errors.Is(completionErr, ErrNoCompletion) && nativeTranscriptPath != "" {
						// Same stdout gap on the recovery turn.
						if recovered, ok := readCopilotCompletionFromSession(req.Mode, nativeTranscriptPath, req.MaxTranscriptBytes); ok {
							payload, completionErr = recovered, nil
						}
					}
				}
			}
		}
	}
	out = Outcome{
		Transcript:             result.Transcript,
		RenderedPrompt:         []byte(prompt),
		TranscriptTruncated:    result.TranscriptTruncated,
		TranscriptDroppedBytes: result.TranscriptDroppedBytes,
		Stderr:                 result.Stderr,
	}
	receipts, receiptsCollected, receiptsErr := collectGoobersIOReceipts(req, c.SelfBin)
	out.InputInspectionReceipts = receipts
	out.InputInspectionReceiptsCollected = receiptsCollected
	// #3456: name a registered-but-unusable MCP server instead of letting its
	// tools go silently missing. The claude adapter reads this from a
	// structured system/init event; Copilot has no transcript equivalent, so
	// this reads the CLI's own run log — available because the confinement
	// already pins --log-dir into the workspace. Unconfined runs have no
	// run-scoped log directory, so the diagnostic stays nil rather than
	// guessing from a shared one.
	if confinement != nil {
		out.MCPServerFailures = copilotMCPServerFailures(req, confinement.logDir)
	}
	if receiptsErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("read goobers-io input inspection receipts: %w", receiptsErr))
	}
	if nativeTranscriptPath != "" {
		if native, ok := readCopilotSessionTranscript(nativeTranscriptPath, req.MaxTranscriptBytes); ok {
			out.Metrics = native.metrics
			out.ModelUsage = native.modelUsage
			if err := agentTelemetry.emit(projectAgentEvents(native.data, req)...); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("harness: copilot-cli: project agent telemetry: %w", err))
			}
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

// readCopilotCompletionFromSession recovers a completion envelope from the CLI's
// own session log when the stdout capture came up empty or unparseable.
//
// Copilot does not reliably echo its final assistant message to stdout under
// --silent --output-format=text once MCP tools are attached. The message is
// always written to the session log, so when stdout yields nothing the log still
// holds a well-formed completion. Without this fallback the harness reports
// "Copilot final response is not valid JSON" for a completion the model produced
// correctly, burns the contract-recovery turn on the same stdout gap, and fails
// the stage twice -- stranding work the agent already committed to the branch.
//
// It is deliberately a FALLBACK, not the primary path: stdout remains
// authoritative when present, and this only fires after the normal read has
// already failed with ErrNoCompletion. The recovered payload goes through the
// same extraction and envelope validation as any other completion, so a genuinely
// malformed final message still fails.
func readCopilotCompletionFromSession(mode Mode, path string, limit int64) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	native, ok := readCopilotSessionTranscript(path, limit)
	if !ok || len(native.finalMessage) == 0 {
		return nil, false
	}
	payload := extractCompletionJSON(bytes.TrimSpace(native.finalMessage))
	if !json.Valid(payload) {
		return nil, false
	}
	if err := validateCopilotCompletion(mode, payload); err != nil {
		return nil, false
	}
	return payload, true
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
	// LAST, not first. The capture buffer holds the model's whole final turn,
	// and a tool-using model routinely emits JSON before its completion: shell
	// output like `git status --porcelain=v2`, a pretty-printed config it just
	// read, or its own narration quoting a fragment. Taking the FIRST balanced
	// value returns one of those, which then fails envelope validation and
	// kills the stage with "final response is not valid JSON" even though the
	// model's actual completion — the last value — was perfectly well formed.
	// The completion is by construction what the model ends on.
	if inner, ok := lastJSONValue(trimmed); ok {
		return inner
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

// lastJSONValue returns the LAST balanced, valid JSON object or array in
// payload.
//
// This exists because firstJSONValue picks the wrong value for a tool-using
// model: the captured final turn frequently contains JSON that is NOT the
// completion (shell output the model echoed, a config file it read back, a
// fragment it quoted while narrating), and the completion envelope is what the
// model ends on. Scanning from the end finds the completion; scanning from the
// start finds the noise and fails the stage on a well-formed response.
//
// It scans FORWARD and keeps the last match rather than walking backwards from
// the final closer. A backwards walk cannot cheaply honour string literals --
// a `}` or `]` inside a JSON string ("contains a brace } in prose") corrupts a
// reverse depth count and mismatches the opener. firstJSONValue already does
// correct forward scanning with string/escape handling, so this reuses that
// pass repeatedly over the remaining tail.
func lastJSONValue(payload []byte) ([]byte, bool) {
	var last []byte
	var found bool
	for offset := 0; offset < len(payload); {
		value, start, end, ok := nextJSONValue(payload[offset:])
		if !ok {
			break
		}
		last = value
		found = true
		_ = start
		offset += end
	}
	return last, found
}

// scanBalancedJSONSpan returns the exclusive end offset of the balanced JSON
// object or array beginning at start. String literals and escapes are honoured
// so structural characters inside strings do not affect nesting depth.
func scanBalancedJSONSpan(payload []byte, start int) (end int, ok bool) {
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
				return i + 1, true
			}
		}
	}
	return 0, false
}

// nextJSONValue finds the first balanced, valid JSON object or array in payload
// and reports it along with its start and end offsets (end is exclusive).
// When a balanced structural candidate is invalid, scanning resumes after its
// opener so it cannot hide a later valid value.
func nextJSONValue(payload []byte) (value []byte, start, end int, ok bool) {
	searchFrom := 0
	for {
		rel := bytes.IndexAny(payload[searchFrom:], "{[")
		if rel < 0 {
			return nil, 0, 0, false
		}
		start = searchFrom + rel
		end, balanced := scanBalancedJSONSpan(payload, start)
		if !balanced {
			return nil, 0, 0, false
		}
		candidate := payload[start:end]
		if json.Valid(candidate) {
			return candidate, start, end, true
		}
		searchFrom = start + 1
	}
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
	end, ok := scanBalancedJSONSpan(payload, start)
	if !ok {
		return nil, false
	}
	candidate := payload[start:end]
	if !json.Valid(candidate) {
		return nil, false
	}
	return candidate, true
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
func (c *CopilotAdapter) credentialEnv(ctx context.Context, ephemeralTmp *ephemeraltmp.Scope, req RunRequest) ([]string, error) {
	return buildCredentialEnv(ctx, credentialEnvConfig{
		adapterName:                    c.Name(),
		envCapabilities:                c.EnvCapabilities,
		optionalCredentialCapabilities: c.OptionalCredentialCapabilities,
		extraEnvAllowlist:              c.ExtraEnvAllowlist,
		instanceRoot:                   c.InstanceRoot,
		selfBin:                        c.SelfBin,
		ephemeralTmp:                   ephemeralTmp,
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
