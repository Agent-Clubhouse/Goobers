package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ReadinessCheck is a value-free observation, separate from workload execution.
// Details deliberately omit arbitrary subprocess output and error strings.
type ReadinessCheck struct {
	Category string `json:"category"`
	Code     string `json:"code"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail"`
}

// Readiness reports only the current process's bounded, read-only observations.
type Readiness struct {
	Executable string           `json:"executable,omitempty"`
	Version    string           `json:"version,omitempty"`
	Checks     []ReadinessCheck `json:"checks"`
}

// ReadinessTimeout bounds each executable or authentication subprocess probe.
const ReadinessTimeout = 10 * time.Second

func readinessCheck(category, code, outcome, detail string) ReadinessCheck {
	return ReadinessCheck{Category: category, Code: code, Outcome: outcome, Detail: detail}
}

// ProbeReadiness never calls Run or resolves configured model credentials. It
// reuses Preflight only for adapters with a known non-model authentication
// path; Copilot's prompt-based auth fallback is deliberately not invoked.
func ProbeReadiness(ctx context.Context, adapter Adapter, spec apiv1.GooberSpec, configuredCredential bool) Readiness {
	report := Readiness{}
	report.Checks = append(report.Checks, readinessConfig(adapter, spec))
	command, runner, env := readinessProcess(adapter)
	if len(command) == 0 {
		report.Checks = append(report.Checks, readinessCheck("unavailable_tool", "harness_executable_unobservable", "unobservable", "adapter exposes no read-only executable probe"))
	} else {
		report.probeVersion(ctx, command, runner, env)
	}
	report.Checks = append(report.Checks, readinessAuthentication(ctx, adapter, spec, configuredCredential))
	report.Checks = append(report.Checks, readinessCheck("headless", "harness_headless_unobservable", "unobservable", "version and authentication status do not prove headless model invocation; no model session started"))
	return report
}

func readinessConfig(adapter Adapter, spec apiv1.GooberSpec) ReadinessCheck {
	var err error
	switch a := adapter.(type) {
	case *CopilotAdapter:
		// ResolveConfig may discover models or negotiate a launcher. Validate only
		// local shape here; no live catalogue means no model-support assertion.
		var optionsErr error
		options, fallback, optionsErr := copilotFallbackOption(spec.HarnessOptions)
		if optionsErr == nil {
			_, err = normalizeResolvedCopilotConfig(spec.Model, options)
		} else {
			err = optionsErr
		}
		if fallback && spec.Model == "" {
			err = errors.New("fallback requires model")
		}
		if err == nil {
			return readinessCheck("capability_mismatch", "harness_model_unobservable", "unobservable", "option shape accepted; model availability and model-dependent options require live catalogue evidence")
		}
	case *ClaudeAdapter:
		err = a.ValidateConfig(spec.Model, spec.HarnessOptions)
	case *CodexAdapter:
		err = a.ValidateConfig(spec.Model, spec.HarnessOptions)
	default:
		return readinessCheck("capability_mismatch", "harness_model_unobservable", "unobservable", "adapter exposes no known read-only configuration validator")
	}
	if err != nil {
		return readinessCheck("capability_mismatch", "harness_configuration_mismatch", "failed", "adapter rejected selected model or options; diagnostic values omitted")
	}
	return readinessCheck("capability_mismatch", "harness_configuration_supported", "supported", "adapter static model/options contract accepted; provider model entitlement unverified")
}

func readinessProcess(adapter Adapter) ([]string, ProcessRunner, []string) {
	switch a := adapter.(type) {
	case *CopilotAdapter:
		return a.Command, a.runner(), baseEnv(a.ExtraEnvAllowlist, a.EnvUnset)
	case *ClaudeAdapter:
		return a.Command, a.runner(), baseEnv(a.ExtraEnvAllowlist, a.EnvUnset)
	case *CodexAdapter:
		return a.Command, a.runner(), baseEnv(a.ExtraEnvAllowlist, a.EnvUnset)
	default:
		return nil, nil, nil
	}
}

var readinessVersion = regexp.MustCompile(`\b[0-9]+\.[0-9]+\.[0-9]+\b`)

func (report *Readiness) probeVersion(ctx context.Context, command []string, runner ProcessRunner, env []string) {
	path, err := exec.LookPath(command[0])
	if err != nil {
		report.Checks = append(report.Checks, readinessCheck("unavailable_tool", "harness_executable_unavailable", "failed", "configured executable unavailable on reporting-process PATH"))
		return
	}
	report.Executable = path
	// Wrappers can run arbitrary code before dispatching their argument. Only
	// their --version interface is exercised; no handshake or prompt is used.
	command = append(append([]string(nil), resolveHarnessCommand(command)...), "--version")
	probeCtx, cancel := context.WithTimeout(ctx, ReadinessTimeout)
	defer cancel()
	stdout := newTranscriptBuffer(maxPreflightDiagnosticBytes)
	result, err := runner.Run(probeCtx, ProcessRequest{Command: command, Env: env, MaxTranscriptBytes: maxPreflightDiagnosticBytes, StdoutCapture: stdout})
	if err != nil || result.ExitCode != 0 {
		report.Checks = append(report.Checks, readinessFailure("version", err))
		return
	}
	// A version line is untrusted transcript too: retain only its numeric
	// version, never arbitrary text, paths, tokens, prompts or diagnostics.
	report.Version = readinessVersion.FindString(string(stdout.Bytes()))
	if report.Version == "" || stdout.Truncated() {
		report.Checks = append(report.Checks, readinessCheck("unavailable_tool", "harness_version_unobservable", "unobservable", "bounded stdout did not contain a numeric version"))
		return
	}
	report.Checks = append(report.Checks, readinessCheck("unavailable_tool", "harness_executable_available", "supported", "executable version observed in reporting process"))
}

func readinessAuthentication(ctx context.Context, adapter Adapter, spec apiv1.GooberSpec, configured bool) ReadinessCheck {
	if codex, ok := adapter.(*CodexAdapter); ok && CodexUsesAmbientChatGPT(spec.HarnessOptions) {
		return readinessCodexAuthentication(ctx, codex, spec)
	}
	if configured {
		return readinessCheck("authentication", "harness_authentication_unobservable", "unobservable", "configured model credential was not resolved; ambient authentication cannot substitute for it")
	}
	// Claude's existing Preflight uses --version and auth status, never a model
	// prompt. Construct a fresh adapter with no credential resolver, preserving
	// the environment policy and process runner used by actual execution.
	a, ok := adapter.(*ClaudeAdapter)
	if !ok {
		return readinessCheck("authentication", "harness_authentication_unobservable", "unobservable", "adapter has no proven non-model authentication-usability probe")
	}
	runner := &readinessAuthRunner{runner: a.runner(), phase: "version"}
	probe := &ClaudeAdapter{Command: a.Command, Runner: runner, ExtraEnvAllowlist: a.ExtraEnvAllowlist, EnvUnset: a.EnvUnset}
	probeCtx, cancel := context.WithTimeout(ctx, ReadinessTimeout)
	defer cancel()
	_, err := probe.Preflight(probeCtx)
	if err != nil {
		return readinessFailure(runner.phase, err)
	}
	return readinessCheck("authentication", "harness_authentication_ready", "supported", "ambient authentication accepted by read-only adapter preflight; provider authorization unverified")
}

func readinessFailure(phase string, err error) ReadinessCheck {
	var execErr *exec.Error
	var pathErr *os.PathError
	if errors.Is(err, ErrTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &execErr) || errors.As(err, &pathErr) {
		return readinessCheck("transport", "harness_transport_failed", "failed", "bounded harness startup or transport failed; transcript and error omitted")
	}
	if errors.Is(err, ErrCanceled) || errors.Is(err, context.Canceled) {
		return readinessCheck("unobservable", "harness_probe_unobservable", "unobservable", "probe canceled before readiness could be established")
	}
	if phase == "authentication" {
		return readinessCheck("authentication", "harness_authentication_failed", "failed", "read-only authentication status rejected; transcript and error omitted")
	}
	return readinessCheck("transport", "harness_transport_failed", "failed", "bounded version startup failed; transcript and error omitted")
}

// Classify using the executed operation, never words in an untrusted error.
type readinessAuthRunner struct {
	runner ProcessRunner
	phase  string
}

func (r *readinessAuthRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	n := len(req.Command)
	if n >= 2 && req.Command[n-2] == "auth" && req.Command[n-1] == "status" {
		r.phase = "authentication"
	}
	return r.runner.Run(ctx, req)
}

func readinessCodexAuthentication(ctx context.Context, adapter *CodexAdapter, spec apiv1.GooberSpec) ReadinessCheck {
	config, err := normalizeCodexConfig(spec.Model, spec.HarnessOptions)
	if err != nil || len(adapter.Command) == 0 {
		return readinessCheck("authentication", "harness_authentication_unobservable", "unobservable", "invalid Codex authentication configuration")
	}
	command := append(append([]string(nil), resolveHarnessCommand(adapter.Command)...), "login", "status")
	if !config.allowFileBackedCredentials {
		command = append(command, "-c", `cli_auth_credentials_store="keyring"`)
	}
	probeCtx, cancel := context.WithTimeout(ctx, ReadinessTimeout)
	defer cancel()
	result, err := adapter.runner().Run(probeCtx, ProcessRequest{Command: command, Env: ambientCodexEnv(adapter.ExtraEnvAllowlist, adapter.EnvUnset), MaxTranscriptBytes: maxPreflightDiagnosticBytes})
	if err != nil || result.ExitCode != 0 {
		return readinessFailure("authentication", err)
	}
	if !strings.Contains(strings.ToLower(string(result.Transcript)), "logged in using chatgpt") {
		return readinessCheck("authentication", "harness_authentication_unobservable", "unobservable", "Codex did not confirm an ambient ChatGPT session")
	}
	return readinessCheck("authentication", "harness_authentication_ready", "supported", "Codex reports an ambient ChatGPT session; provider authorization unverified")
}
