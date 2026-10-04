package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// maxSuccessLineBytes bounds how much of one stdout line the success-line
// watcher buffers; a longer line can never equal the short success line.
const maxSuccessLineBytes = 256

// successLineWatcher observes a probe's stdout and fires onSeen the first time
// a complete line equals want (trimmed, case-insensitive).
type successLineWatcher struct {
	want   string
	onSeen func()

	mu       sync.Mutex
	line     []byte
	overflow bool
	seen     atomic.Bool
}

func (w *successLineWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b != '\n' {
			if len(w.line) < maxSuccessLineBytes {
				w.line = append(w.line, b)
			} else {
				w.overflow = true
			}
			continue
		}
		if !w.overflow && strings.EqualFold(strings.TrimSpace(string(w.line)), w.want) && !w.seen.Swap(true) {
			w.onSeen()
		}
		w.line, w.overflow = w.line[:0], false
	}
	return len(p), nil
}

// authProbeSuccessLine is the early-success line for this probe, or "" to
// wait for the process to exit. A launcher's declared probe has its own
// contract, and adapter-managed session verification needs the CLI to finish
// writing its session transcript, so neither may be cut short.
func (c *CopilotAdapter) authProbeSuccessLine(contract launcherContract, verifyAdapterManagedSession bool) string {
	if contract.AuthProbe != nil || verifyAdapterManagedSession {
		return ""
	}
	return c.AuthCheckSuccessLine
}

// runProbeUntilSuccessLine runs req and, when successLine is set, ends it as
// soon as its stdout carries that line. The Copilot CLI can keep running well
// after printing a successful reply (session teardown, telemetry flush); on
// slow hosts that tail alone pushed a valid sign-in probe past the preflight
// deadline and was then misreported as an authentication failure (#5165). The
// reply is the evidence the probe exists to collect, so once it is seen the
// remaining process tree is killed and the probe counts as passed — even if
// the process would later have exited non-zero, since a model reply already
// proves the sign-in this probe checks.
func runProbeUntilSuccessLine(ctx context.Context, runner ProcessRunner, req ProcessRequest, successLine string) (ProcessResult, error) {
	if successLine == "" {
		return runner.Run(ctx, req)
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcher := &successLineWatcher{want: successLine, onSeen: cancel}
	if req.StdoutCapture != nil {
		req.StdoutCapture = io.MultiWriter(watcher, req.StdoutCapture)
	} else {
		req.StdoutCapture = watcher
	}
	res, err := runner.Run(probeCtx, req)
	if watcher.seen.Load() {
		res.ExitCode = 0
		return res, nil
	}
	return res, err
}

const (
	copilotPreflightHomePrefix = "goobers-copilot-preflight-"
	copilotConfigFileName      = "config.json"
)

func (c *CopilotAdapter) authPreflightPlan(contract launcherContract) ([]string, bool, error) {
	args := c.AuthCheckArgs
	if contract.AuthProbe != nil {
		args = append(slices.Clone(contract.AuthProbe.Args), c.AuthProbeExtraArgs...)
	}
	verifyAdapterManagedSession := c.VerifyAdapterManagedSession &&
		contract.SessionMode == "adapter-managed" &&
		contract.AuthProbe == nil
	if verifyAdapterManagedSession && len(args) == 0 {
		return nil, false, fmt.Errorf("harness: copilot-cli: launcher session verification requires an authentication probe")
	}
	return args, verifyAdapterManagedSession, nil
}

// prepareCopilotPreflightEnvironment isolates the authentication probe from
// ambient extensions, plugins, MCP servers, hooks, and instructions. When no
// explicit model token is available, config.json is copied so the CLI can
// still use its stored login without inheriting the rest of the user profile.
func prepareCopilotPreflightEnvironment(env []string, seedStoredAuth bool) ([]string, string, func(), error) {
	ambientHome, hasAmbientHome := copilotConfigHome(env)
	home, err := os.MkdirTemp("", copilotPreflightHomePrefix)
	if err != nil {
		return nil, "", nil, fmt.Errorf("create isolated Copilot preflight home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(home) }

	if seedStoredAuth && hasAmbientHome {
		source := filepath.Join(ambientHome, copilotConfigFileName)
		data, readErr := os.ReadFile(source)
		switch {
		case readErr == nil:
			if err := os.WriteFile(filepath.Join(home, copilotConfigFileName), data, 0o600); err != nil {
				cleanup()
				return nil, "", nil, fmt.Errorf("seed isolated Copilot preflight authentication: %w", err)
			}
		case !os.IsNotExist(readErr):
			cleanup()
			return nil, "", nil, fmt.Errorf("read stored Copilot authentication: %w", readErr)
		}
	}

	env = overrideEnv(env, "COPILOT_HOME", home)
	env = removeEnvironment(env, copilotWorkspaceMCPEnv)
	env = removeEnvironment(env, copilotPluginDirOnlyEnv)
	env = append(env, copilotPluginDirOnlyEnv+"=true")
	return env, home, cleanup, nil
}

func shouldRetryCopilotLauncherAuthProbe(ctx context.Context, result ProcessResult, runErr error, launcherProbe bool) bool {
	if !launcherProbe {
		return false
	}
	if errors.Is(runErr, ErrCanceled) || errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return false
	}
	return copilotLauncherAuthProbeLooksTransient(ctx, result, runErr)
}

func copilotLauncherAuthProbeLooksTransient(ctx context.Context, result ProcessResult, runErr error) bool {
	if copilotAuthProbeLooksLikeCredentialFailure(result.Transcript) {
		return false
	}
	if errors.Is(runErr, ErrTimeout) || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	if result.ExitCode == -1 && copilotLauncherAuthProbeProcessStartError(runErr) {
		return true
	}
	return copilotAuthProbeHasLauncherBootstrapSignal(result.Transcript)
}

func copilotLauncherAuthProbeProcessStartError(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return true
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return false
	}
	op := strings.ToLower(pathErr.Op)
	return strings.Contains(op, "exec") || strings.Contains(op, "createprocess")
}

func copilotAuthProbeHasLauncherBootstrapSignal(transcript []byte) bool {
	text := strings.ToLower(string(transcript))
	for _, marker := range []string{
		"bootstrap failed",
		"failed before auth status completed",
		"failed to start session",
		"launcher bootstrap",
		"launcher process failed",
		"mcp bootstrap",
		"mcp startup",
		"session bootstrap",
		"session startup",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func copilotAuthProbeLooksLikeCredentialFailure(transcript []byte) bool {
	text := strings.ToLower(string(transcript))
	for _, marker := range []string{
		"auth failed",
		"authentication failed",
		"credential",
		"forbidden",
		"invalid or revoked",
		"log in",
		"login",
		"not authenticated",
		"not logged in",
		"not signed in",
		"sign in",
		"signed out",
		"token",
		"unauthorized",
		"401",
		"403",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
