package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// DefaultTimeout bounds a shell stage's execution when neither the executor
// nor the stage declares one.
const DefaultTimeout = 10 * time.Minute

// DefaultMaxOutputBytes caps captured stdout/stderr (each stream) when
// neither the executor nor the stage declares a limit.
const DefaultMaxOutputBytes int64 = 1 << 20 // 1 MiB

// groupKillWaitDelay bounds how long Run waits for cmd.Wait() to return
// after killing the whole process group on timeout, in case a descendant
// escaped the group (e.g. via setsid) and is still holding a stdout/stderr
// pipe open — cmd.Wait() would otherwise never return, hanging the stage
// (and graceful drain) exactly as before the group-kill fix, just one layer
// down (#119). Giving up after this bound lets the stage's own accounting
// proceed even though the escaped process may leak; there is no portable,
// unconditional way to guarantee its death. Mirrors internal/harness's
// identical constant (a second, small copy — not worth a shared package for
// one duration value, same tradeoff already accepted for fsyncDir this
// wave).
const groupKillWaitDelay = 5 * time.Second

// Well-known Task.Inputs keys a deterministic shell stage may declare. These
// travel through InvocationEnvelope.Inputs rather than as DeterministicRun
// fields — see doc.go.
const (
	// InputTimeout is a time.ParseDuration string, e.g. "5m".
	InputTimeout = "timeout"
	// InputResultFile is a path, relative to the workspace, whose bytes (once
	// the command exits) become an artifact. If declared, the file's presence
	// is also a success criterion: a zero exit with no such file is a failure.
	// If those bytes also parse as a flat JSON object, its string/number/bool
	// fields are additionally merged into ResultEnvelope.Outputs (in addition
	// to, not instead of, recording the raw bytes as an artifact) — this is
	// how a shell subcommand (a real OS subprocess, not an in-process
	// invoke.Deterministic) reports structured handoff data a downstream
	// task's Task.InputsFrom can reference, e.g. `goobers open-pr`'s prNumber
	// (#132). Not JSON, or not a flat object, is not an error: the artifact/
	// presence-check contract holds regardless.
	InputResultFile = "resultFile"
	// InputMaxOutputBytes is a decimal integer overriding the per-stream
	// output cap.
	InputMaxOutputBytes = "maxOutputBytes"
)

// OutputNoWork is the well-known InputResultFile output key a deterministic
// command sets to boolean true to report ResultNoWork instead of
// ResultSuccess (issue #233): the command exited 0 (it did not error) and
// its declared result file was present and parsed as JSON, but it found
// nothing to act on this tick (e.g. `goobers backlog-query --claim` with an
// empty or fully-contested eligible set). Checked only after a successful
// declared-result-file read, so this is an explicit, structured signal, not
// an exit-code convention every unrelated shell stage would have to avoid
// colliding with. A command with no declared InputResultFile has no way to
// signal ResultNoWork — only ResultSuccess (exit 0) or ResultFailure — since
// there is nowhere else fail-closed to read a structured signal from.
const OutputNoWork = "noWork"

// OutputErrorCode / OutputErrorMessage / OutputErrorRetryable are the
// well-known InputResultFile output keys a deterministic command sets to
// report a TYPED failure — the failure analog of OutputNoWork (#614). A
// command that exits nonzero after writing its declared result file with
// OutputErrorCode set gets that code (and message/retryable, when present)
// as the stage's ErrorInfo instead of the generic nonzero_exit — and,
// because the file exists, instead of the missing_result_file that used to
// bury the real cause (e.g. a GitHub rate-limit 403 now journals as
// github_rate_limited with the reset time in its message). Checked only on
// a nonzero exit with a successfully read result file, so an unrelated
// stage that never writes these keys keeps exactly the old behavior.
const (
	OutputErrorCode      = "errorCode"
	OutputErrorMessage   = "errorMessage"
	OutputErrorRetryable = "errorRetryable"
)

// ArtifactRecorder persists stage output bytes into the run journal and
// returns a content-addressed pointer to them. *journal.Run satisfies this.
type ArtifactRecorder interface {
	RecordArtifact(name string, data []byte) (journal.Ref, error)
}

// ShellExecutor runs deterministic shell stages (invoke.Deterministic) in the
// worktree the caller hands it via InvocationEnvelope.Workspace.
type ShellExecutor struct {
	// Injector resolves capability-scoped credentials for a stage's declared
	// capabilities. Required.
	Injector *credentials.Injector
	// Journal records captured output and declared result files as
	// content-addressed artifacts. Required.
	Journal ArtifactRecorder
	// DefaultTimeout overrides the package DefaultTimeout when positive.
	DefaultTimeout time.Duration
	// DefaultMaxOutputBytes overrides the package DefaultMaxOutputBytes when
	// positive.
	DefaultMaxOutputBytes int64
	// InstanceRoot, if set, is passed to every stage process as
	// GOOBERS_INSTANCE_ROOT — the only way a `goobers` CLI subcommand invoked
	// as a stage's command (its cwd is the stage's worktree, not the instance
	// root) can locate instance.yaml/config/scheduler (#131/#132). Empty by
	// default: a caller that never sets it (e.g. an existing test) gets
	// unchanged behavior — no such var is set.
	InstanceRoot string
	// SelfBin, if set, is the absolute path substituted for a bare "goobers"
	// command token before exec. Deterministic stages declare their command as
	// e.g. ["goobers", "backlog-query", …], but a stage runs with cwd set to a
	// fresh worktree clone that never contains the (gitignored, uncommitted)
	// goobers binary, and a bare name is PATH-resolved against the *daemon's*
	// PATH — not the worktree — so "goobers" fails at exec (#229). Wiring sets
	// this once from os.Executable() so a stage execs the exact same binary as
	// the running daemon (no version skew). Empty by default: an unset caller
	// runs the command verbatim (unchanged behavior).
	SelfBin string
}

// NewShellExecutor builds a ShellExecutor. injector and journal must not be
// nil: a nil injector could silently skip capability admission, and a nil
// journal would leave captured output unrecorded — both fail closed here
// rather than at first use.
func NewShellExecutor(injector *credentials.Injector, rec ArtifactRecorder) (*ShellExecutor, error) {
	if injector == nil {
		return nil, errors.New("executor: injector must not be nil")
	}
	if rec == nil {
		return nil, errors.New("executor: journal must not be nil")
	}
	return &ShellExecutor{Injector: injector, Journal: rec}, nil
}

// stageInvokesGoobersCLI reports whether a stage's command is the goobers CLI
// itself (e.g. backlog-query/open-pr/ci-poll/issue-close-out) rather than an
// external tool (make, go, git). It is the single discriminator for two
// goobers-CLI-specific behaviors: substituting the daemon's own binary for the
// bare "goobers" token (SelfBin, #229), and injecting the run's operational
// identity into the stage env (#322). A stage that runs the project's own
// build/test suite (`make ci`) is not a goobers-CLI stage on either axis.
func stageInvokesGoobersCLI(command []string) bool {
	return len(command) > 0 && command[0] == "goobers"
}

// stageInvokesProviderBuiltin narrows transient stderr classification to the
// built-in stages that call a provider. Other goobers subcommands can fail
// with similar words but have separate retry contracts.
func stageInvokesProviderBuiltin(command []string) bool {
	if !stageInvokesGoobersCLI(command) || len(command) < 2 {
		return false
	}
	switch command[1] {
	case "apply-verdict",
		"backlog-query",
		"gather-pr-context",
		"gather-sibling-context",
		"issue-close-out",
		"merge-pr",
		"open-pr",
		"post-merge",
		"pr-select",
		"rebase-pr",
		"remediation-checkpoint":
		return true
	default:
		return false
	}
}

// Run implements invoke.Deterministic. It executes run.Command in
// env.Workspace with a capability-scoped, non-ambient environment, enforces a
// timeout by killing the whole process group, captures size-bounded and
// secret-scrubbed stdout/stderr as artifacts, and — if InputResultFile is
// declared — lifts that file into an artifact and requires its presence for
// success.
//
// A non-nil error means the executor itself could not produce a result
// (misconfiguration, credential resolution failure, a journal write failure,
// or a transient built-in provider outage) — ARCHITECTURE.md invariant 6, fail
// closed rather than degrade. Other declared-command failures are normal
// ResultFailure envelopes.
func (e *ShellExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if len(run.Command) == 0 {
		return apiv1.ResultEnvelope{}, errors.New("executor: DeterministicRun declares no command")
	}
	if env.Workspace == "" {
		// exec.Cmd treats Dir == "" as "run in the daemon's own working
		// directory" — a silent, surprising fallback (#122) rather than the
		// fail-closed misconfiguration error an unset workspace should be.
		return apiv1.ResultEnvelope{}, errors.New("executor: InvocationEnvelope.Workspace is empty")
	}
	timeout, err := e.timeoutFor(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	maxOutput, err := e.maxOutputFor(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	resultFile := stringInput(env, InputResultFile)

	registry, scrubber := journal.DefaultScrubber()
	// Only a stage whose command IS the goobers CLI receives the run's
	// operational identity (GOOBERS_RUN_ID etc.). A stage that runs the
	// project's own build/test suite (local-ci's `make ci` → `go test ./...`)
	// must not inherit it, or — in a self-hosting project — the runner's live
	// run env leaks into its own test suite (#322). This is the same
	// command[0]=="goobers" discriminator the SelfBin substitution uses below:
	// the goobers-CLI-stage-ness of a stage is what decides both.
	injectRunContext := stageInvokesGoobersCLI(run.Command)
	stageEnv, err := buildStageEnv(ctx, e.Injector, env.Capabilities, registry, env.RunID, env.WorkflowID, e.InstanceRoot, injectRunContext, env.Inputs)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: resolve credentials: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Substitute the running daemon's own binary for a bare "goobers" token: the
	// stage's cwd is a fresh worktree clone that never contains the goobers
	// binary, and a bare name would PATH-resolve against the daemon's PATH, not
	// the worktree — so it fails at exec (#229). SelfBin is byte-identical to the
	// running daemon, avoiding version skew.
	name := run.Command[0]
	if e.SelfBin != "" && stageInvokesGoobersCLI(run.Command) {
		name = e.SelfBin
	}
	cmd := exec.Command(name, run.Command[1:]...)
	cmd.Dir = env.Workspace
	cmd.Env = stageEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := configureCommandNetwork(cmd, run.Network); err != nil {
		return apiv1.ResultEnvelope{}, err
	}

	stdout := &capturingWriter{limit: maxOutput}
	stderr := &capturingWriter{limit: maxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Error:   &apiv1.ErrorInfo{Code: "exec_start", Message: err.Error(), Retryable: false},
			Summary: fmt.Sprintf("failed to start %q", run.Command[0]),
		}, nil
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var timedOut, canceled bool
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-runCtx.Done():
		// runCtx.Done() fires both when its own timeout elapses and when the
		// caller's ctx is canceled out from under it — distinguishing the two
		// via context.Cause matters even though only the timeout path is
		// reachable today (internal/runner's dispatch always uses
		// context.WithoutCancel): a future hard-shutdown path that DOES
		// cancel ctx must not be mislabeled as a retryable timeout (#122).
		if errors.Is(context.Cause(runCtx), context.DeadlineExceeded) {
			timedOut = true
		} else {
			canceled = true
		}
		// Kill the whole process group (negative pid), not just the direct
		// child, so a runaway subprocess tree can't outlive the stage.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case waitErr = <-waitDone:
		case <-time.After(groupKillWaitDelay):
			// A descendant escaped the process group (e.g. via setsid) and
			// is still holding a stdout/stderr pipe open, so cmd.Wait()
			// never returns (#119) — give up waiting rather than hang the
			// stage (and graceful drain) forever. waitErr stays nil here,
			// but it's only read below in the non-timeout/non-canceled path,
			// so this bound never masks a real exit code.
		}
	}

	outBytes := scrubber.Scrub(stdout.Bytes())
	errBytes := scrubber.Scrub(stderr.Bytes())

	result := apiv1.ResultEnvelope{Outputs: map[string]interface{}{}, Metrics: map[string]float64{}}

	stdoutRef, err := e.Journal.RecordArtifact(env.TaskID+"/stdout.log", outBytes)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record stdout: %w", err)
	}
	result.Artifacts = append(result.Artifacts, refToPointer(stdoutRef, "text/plain"))
	if stdout.Truncated() {
		result.Outputs["stdoutTruncated"] = true
	}

	stderrRef, err := e.Journal.RecordArtifact(env.TaskID+"/stderr.log", errBytes)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record stderr: %w", err)
	}
	result.Artifacts = append(result.Artifacts, refToPointer(stderrRef, "text/plain"))
	if stderr.Truncated() {
		result.Outputs["stderrTruncated"] = true
	}

	if timedOut {
		if stageInvokesProviderBuiltin(run.Command) {
			return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(fmt.Errorf(
				"executor: provider stage %q exceeded timeout %s: %w",
				run.Command[1], timeout, context.DeadlineExceeded,
			))
		}
		result.Status = apiv1.ResultFailure
		result.Error = &apiv1.ErrorInfo{
			Code:      "timeout",
			Message:   fmt.Sprintf("stage exceeded timeout %s", timeout),
			Retryable: true,
		}
		result.Summary = "stage timed out and was killed"
		return result, nil
	}
	if canceled {
		// Distinct from "timeout": the stage's own deadline had not elapsed —
		// its context was canceled for some other reason (unreachable today,
		// see the select above's doc comment). Not retryable: unlike a
		// transient timeout, a deliberate cancellation should not be retried
		// the same way.
		result.Status = apiv1.ResultFailure
		result.Error = &apiv1.ErrorInfo{
			Code:      "canceled",
			Message:   "stage's context was canceled (not a timeout)",
			Retryable: false,
		}
		result.Summary = "stage was canceled"
		return result, nil
	}

	exitCode := exitCodeOf(waitErr)
	result.Metrics["exitCode"] = float64(exitCode)

	if exitCode != 0 && stageInvokesProviderBuiltin(run.Command) {
		// #control precedence ruling (2026-07-17, the #613/#711/#712
		// chokepoint): a provider-builtin stage that got far enough to
		// self-report structurally via its declared result file
		// (failProviderStage's OutputErrorCode, #614) is a richer, more
		// specific signal than raw stderr text — use it, and skip stderr
		// classification entirely, so #711's fine-grained codes and #712's
		// result.Outputs["rateLimitReset"] read stay authoritative instead
		// of being silently reclassified by this intercept. Only fall
		// through to stderr-text classification below when no structured
		// result exists at all — the residual case this intercept actually
		// exists for: a stage that died before it could call
		// failProviderStage (bad flags, signal kill, panic).
		if resultFile != "" {
			if full, perr := apiv1.ResolveContainedPath(env.Workspace, resultFile); perr == nil {
				if data, rerr := os.ReadFile(full); rerr == nil {
					ref, aerr := e.Journal.RecordArtifact(env.TaskID+"/result", scrubber.Scrub(data))
					if aerr != nil {
						return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record result file: %w", aerr)
					}
					result.Artifacts = append(result.Artifacts, refToPointer(ref, mediaTypeFor(resultFile)))
					mergeResultFileOutputs(&result, data)
					if code, ok := result.Outputs[OutputErrorCode].(string); ok && code != "" {
						message, _ := result.Outputs[OutputErrorMessage].(string)
						if message == "" {
							message = fmt.Sprintf("command exited %d", exitCode)
						}
						if retryable, _ := result.Outputs[OutputErrorRetryable].(bool); retryable {
							return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(fmt.Errorf(
								"executor: provider stage %q reported %s: %s", run.Command[1], code, message,
							))
						}
						result.Status = apiv1.ResultFailure
						result.Error = &apiv1.ErrorInfo{Code: code, Message: message, Retryable: false}
						result.Summary = message
						return result, nil
					}
					// The file existed and parsed but carried no
					// OutputErrorCode (the stage self-reported success
					// shape yet still exited nonzero, or wrote an
					// unrelated result) — its artifact/outputs stay
					// attached to result either way; fall through to
					// stderr classification below for the actual verdict.
				}
				// A read error (including not-yet-written, the common
				// crashed-before-writing case) is not fatal here — falls
				// through to stderr classification, exactly as before this
				// check existed.
			}
		}
		message := lastNonEmptyLine(errBytes)
		if message == "" {
			message = fmt.Sprintf("command exited %d", exitCode)
		}
		providerErr := errors.New(message)
		if providers.IsTransientError(providerErr) {
			return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(fmt.Errorf(
				"executor: provider stage %q failed: %w", run.Command[1], providerErr,
			))
		}
		result.Status = apiv1.ResultFailure
		result.Error = &apiv1.ErrorInfo{
			Code:      "provider_error",
			Message:   providerErr.Error(),
			Retryable: false,
		}
		result.Summary = fmt.Sprintf("provider stage %q failed", run.Command[1])
		return result, nil
	}

	if resultFile != "" {
		full, perr := apiv1.ResolveContainedPath(env.Workspace, resultFile)
		switch {
		case perr == nil:
			data, rerr := os.ReadFile(full)
			switch {
			case rerr == nil:
				ref, aerr := e.Journal.RecordArtifact(env.TaskID+"/result", scrubber.Scrub(data))
				if aerr != nil {
					return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record result file: %w", aerr)
				}
				result.Artifacts = append(result.Artifacts, refToPointer(ref, mediaTypeFor(resultFile)))
				mergeResultFileOutputs(&result, data)
			case os.IsNotExist(rerr):
				result.Status = apiv1.ResultFailure
				result.Error = missingResultFileError(resultFile, exitCode, waitErr, errBytes)
				result.Summary = "declared result file missing"
				return result, nil
			default:
				// rerr here is an *fs.PathError (or similar) wrapping the
				// underlying syscall.Errno — %w already carries that errno
				// text (e.g. "permission denied") into this executor-level
				// error, which internal/runner's runTask journals verbatim
				// as an executor_error event (#711): no separate logging
				// needed, the errno reaches the run journal through the
				// normal error-propagation path.
				return apiv1.ResultEnvelope{}, fmt.Errorf("executor: read result file %q: %w", resultFile, rerr)
			}
		case errors.Is(perr, os.ErrNotExist):
			// A missing component in the declared path resolves the same way
			// EvalSymlinks reports a plain missing file — same UX as above.
			result.Status = apiv1.ResultFailure
			result.Error = missingResultFileError(resultFile, exitCode, waitErr, errBytes)
			result.Summary = "declared result file missing"
			return result, nil
		case errors.Is(perr, apiv1.ErrPathEscape), errors.Is(perr, apiv1.ErrSymlinkEscape):
			// Untrusted declared path (#120): escapes the workspace lexically
			// or via a symlink. Fail the stage closed, never follow it.
			result.Status = apiv1.ResultFailure
			result.Error = &apiv1.ErrorInfo{
				Code:      "result_file_path_escape",
				Message:   fmt.Sprintf("declared result file %q escapes the workspace: %v", resultFile, perr),
				Retryable: false,
			}
			result.Summary = "declared result file path escapes the workspace"
			return result, nil
		default:
			return apiv1.ResultEnvelope{}, fmt.Errorf("executor: resolve result file %q: %w", resultFile, perr)
		}
	}

	if exitCode == 0 {
		// OutputNoWork (issue #233) only ever downgrades a would-be Success
		// to NoWork — it's read from result.Outputs, which is only ever
		// populated by a successful declared-result-file read above, never
		// on a failure path (those all return early). A stage with no
		// declared resultFile has result.Outputs empty here, so this is a
		// no-op for it.
		if v, ok := result.Outputs[OutputNoWork].(bool); ok && v {
			result.Status = apiv1.ResultNoWork
			result.Summary = "stage found no work to do"
			return result, nil
		}
		result.Status = apiv1.ResultSuccess
		result.Summary = "stage completed"
		return result, nil
	}
	result.Status = apiv1.ResultFailure
	// A typed error reported through the declared result file (see
	// OutputErrorCode) beats the generic nonzero_exit: the command knew
	// exactly why it failed and said so structurally.
	if code, ok := result.Outputs[OutputErrorCode].(string); ok && code != "" {
		message, _ := result.Outputs[OutputErrorMessage].(string)
		if message == "" {
			message = fmt.Sprintf("command exited %d", exitCode)
		}
		retryable, _ := result.Outputs[OutputErrorRetryable].(bool)
		result.Error = &apiv1.ErrorInfo{Code: code, Message: message, Retryable: retryable}
		result.Summary = message
		return result, nil
	}
	result.Error = &apiv1.ErrorInfo{
		Code:      "nonzero_exit",
		Message:   fmt.Sprintf("command exited %d", exitCode),
		Retryable: false,
	}
	result.Summary = fmt.Sprintf("command exited %d", exitCode)
	return result, nil
}

func lastNonEmptyLine(data []byte) string {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func (e *ShellExecutor) timeoutFor(env apiv1.InvocationEnvelope) (time.Duration, error) {
	if s := stringInput(env, InputTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("executor: invalid %s input %q: %w", InputTimeout, s, err)
		}
		return d, nil
	}
	if e.DefaultTimeout > 0 {
		return e.DefaultTimeout, nil
	}
	return DefaultTimeout, nil
}

func (e *ShellExecutor) maxOutputFor(env apiv1.InvocationEnvelope) (int64, error) {
	if s := stringInput(env, InputMaxOutputBytes); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("executor: invalid %s input %q", InputMaxOutputBytes, s)
		}
		return n, nil
	}
	if e.DefaultMaxOutputBytes > 0 {
		return e.DefaultMaxOutputBytes, nil
	}
	return DefaultMaxOutputBytes, nil
}

func stringInput(env apiv1.InvocationEnvelope, key string) string {
	v, ok := env.Inputs[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// mergeResultFileOutputs best-effort-parses a declared result file's bytes as
// a flat JSON object and merges its string/number/bool fields into
// result.Outputs — see InputResultFile's doc comment. data that isn't JSON,
// or isn't a flat object, is silently left alone: the artifact/presence-check
// contract InputResultFile already provides holds either way, and not every
// declared result file is meant to carry structured outputs.
func mergeResultFileOutputs(result *apiv1.ResultEnvelope, data []byte) {
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	for k, v := range m {
		switch v.(type) {
		case string, float64, bool:
			result.Outputs[k] = v
		}
	}
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// missingResultFileStderrExcerptBytes bounds the stderr excerpt
// missingResultFileError attaches (#711) — enough to show the actual cause
// (a stack trace's top frame, a "command not found", a panic message)
// without ballooning the journaled ErrorInfo.Message.
const missingResultFileStderrExcerptBytes = 512

// missingResultFileError builds the diagnostic ErrorInfo for a declared
// result file that was never produced (#711). The bare "was not produced"
// message gave an operator nothing to work with — a command that exited 0
// but forgot to write its file, one that was SIGKILLed mid-run, and one
// whose own logic failed before it ever reached the result-file step all
// looked identical. This distinguishes them: exitCode (Go's exec.ExitError
// convention: -1 when the process died to a signal, not a normal exit) is
// replaced with the actual signal name when the process was signaled
// (signalOf), and a bounded stderr excerpt is appended when the process
// produced any.
func missingResultFileError(resultFile string, exitCode int, waitErr error, errBytes []byte) *apiv1.ErrorInfo {
	detail := fmt.Sprintf("exit code %d", exitCode)
	if sig, ok := signalOf(waitErr); ok {
		detail = fmt.Sprintf("killed by signal %s", sig)
	}
	msg := fmt.Sprintf("declared result file %q was not produced (%s)", resultFile, detail)
	if excerpt := stderrExcerpt(errBytes); excerpt != "" {
		msg += "; stderr: " + excerpt
	}
	return &apiv1.ErrorInfo{Code: "missing_result_file", Message: msg, Retryable: false}
}

// signalOf reports the signal that terminated the process behind waitErr, if
// it died to one (as opposed to a normal, possibly nonzero, exit) — the
// distinction exitCodeOf's -1 sentinel alone loses (a signal death and an
// exec.ExitError of some other unexpected shape both report -1).
func signalOf(waitErr error) (syscall.Signal, bool) {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return 0, false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return ws.Signal(), true
}

// stderrExcerpt returns a bounded, trimmed, "…"-suffixed-when-truncated
// prefix of errBytes (already secret-scrubbed by Run's caller) for
// missingResultFileError. Empty input yields "" so the caller can skip
// appending an empty "; stderr: " clause.
func stderrExcerpt(errBytes []byte) string {
	if len(errBytes) == 0 {
		return ""
	}
	b := errBytes
	truncated := false
	if len(b) > missingResultFileStderrExcerptBytes {
		b = b[:missingResultFileStderrExcerptBytes]
		truncated = true
	}
	s := strings.TrimSpace(string(b))
	if truncated {
		s += "…"
	}
	return s
}

func refToPointer(ref journal.Ref, mediaType string) apiv1.ArtifactPointer {
	return apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, MediaType: mediaType, Size: ref.Size}
}

func mediaTypeFor(path string) string {
	if strings.HasSuffix(path, ".json") {
		return "application/json"
	}
	return "application/octet-stream"
}

// capturingWriter caps total bytes retained from a stream at limit, silently
// discarding (but still acknowledging, so the writer never blocks or errors
// the producing process) anything beyond it.
//
// Write is mutex-guarded because on a give-up timeout (#119's
// groupKillWaitDelay) Run stops waiting on cmd.Wait() while os/exec's own
// stdout/stderr-copying goroutines may still be running (an escaped
// descendant can hold a pipe open indefinitely) — Bytes must not read the
// buffer while such a goroutine could still be writing to it.
type capturingWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func (w *capturingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.truncated {
		return len(p), nil
	}
	remaining := w.limit - int64(w.buf.Len())
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		w.buf.Write(p[:remaining])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// Bytes returns a snapshot of what's been captured so far. Safe to call
// concurrently with Write (see the type doc for why that matters here).
func (w *capturingWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// Truncated reports whether the cap has been hit. Safe to call concurrently
// with Write, for the same reason as Bytes.
func (w *capturingWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}
