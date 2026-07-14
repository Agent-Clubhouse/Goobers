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
	"github.com/goobers/goobers/internal/journal"
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

// Run implements invoke.Deterministic. It executes run.Command in
// env.Workspace with a capability-scoped, non-ambient environment, enforces a
// timeout by killing the whole process group, captures size-bounded and
// secret-scrubbed stdout/stderr as artifacts, and — if InputResultFile is
// declared — lifts that file into an artifact and requires its presence for
// success.
//
// A non-nil error means the executor itself could not produce a result
// (misconfiguration, credential resolution failure, or a journal write
// failure) — ARCHITECTURE.md invariant 6, fail closed rather than degrade.
// Everything the *declared command* can go wrong in (nonzero exit, timeout,
// a missing declared result file) is a normal ResultFailure envelope for the
// runner's retry policy to act on.
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
	stageEnv, err := buildStageEnv(ctx, e.Injector, env.Capabilities, registry, env.RunID, env.WorkflowID, e.InstanceRoot, env.Inputs)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: resolve credentials: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(run.Command[0], run.Command[1:]...)
	cmd.Dir = env.Workspace
	cmd.Env = stageEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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

	var timedOut bool
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-runCtx.Done():
		timedOut = true
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
			// but it's only read below in the non-timeout path, so this
			// bound never masks a real exit code.
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
		result.Status = apiv1.ResultFailure
		result.Error = &apiv1.ErrorInfo{
			Code:      "timeout",
			Message:   fmt.Sprintf("stage exceeded timeout %s", timeout),
			Retryable: true,
		}
		result.Summary = "stage timed out and was killed"
		return result, nil
	}

	exitCode := exitCodeOf(waitErr)
	result.Metrics["exitCode"] = float64(exitCode)

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
				result.Error = &apiv1.ErrorInfo{
					Code:      "missing_result_file",
					Message:   fmt.Sprintf("declared result file %q was not produced", resultFile),
					Retryable: false,
				}
				result.Summary = "declared result file missing"
				return result, nil
			default:
				return apiv1.ResultEnvelope{}, fmt.Errorf("executor: read result file %q: %w", resultFile, rerr)
			}
		case errors.Is(perr, os.ErrNotExist):
			// A missing component in the declared path resolves the same way
			// EvalSymlinks reports a plain missing file — same UX as above.
			result.Status = apiv1.ResultFailure
			result.Error = &apiv1.ErrorInfo{
				Code:      "missing_result_file",
				Message:   fmt.Sprintf("declared result file %q was not produced", resultFile),
				Retryable: false,
			}
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
		result.Status = apiv1.ResultSuccess
		result.Summary = "stage completed"
		return result, nil
	}
	result.Status = apiv1.ResultFailure
	result.Error = &apiv1.ErrorInfo{
		Code:      "nonzero_exit",
		Message:   fmt.Sprintf("command exited %d", exitCode),
		Retryable: false,
	}
	result.Summary = fmt.Sprintf("command exited %d", exitCode)
	return result, nil
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
