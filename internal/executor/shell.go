package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// DefaultTimeout bounds a shell stage's execution when neither the executor
// nor the stage declares one.
const DefaultTimeout = 10 * time.Minute

// DefaultMaxOutputBytes caps captured stdout/stderr (each stream) when
// neither the executor nor the stage declares a limit.
const DefaultMaxOutputBytes int64 = 1 << 20 // 1 MiB

// Well-known Task.Inputs keys a deterministic shell stage may declare. These
// are read by ConfigFromEnvelope; DeterministicRun carries only Command/Image
// at V0 — see doc.go.
const (
	// InputTimeout is a time.ParseDuration string, e.g. "5m".
	InputTimeout = "timeout"
	// InputResultFile is a path, relative to the workspace, whose bytes (once
	// the command exits) become a produced artifact. If declared, the file's
	// presence is also a success criterion: a zero exit with no such file is
	// a failure.
	InputResultFile = "resultFile"
	// InputMaxOutputBytes is a decimal integer overriding the per-stream
	// output cap.
	InputMaxOutputBytes = "maxOutputBytes"
)

// ProducedArtifact is one artifact an executor produced, not yet committed to
// the journal — raw content the caller (ultimately the runner) digests and
// stores. Structurally identical by design to the (separately defined, not
// depended on here) runner.ProducedArtifact, so converting between them is a
// one-line loop wherever that seam is wired.
type ProducedArtifact struct {
	// Name is the artifact's logical handle — becomes the ContextPointer.Name
	// a downstream stage sees this artifact under.
	Name string
	// Data is the raw artifact bytes, already locally scrubbed of any
	// credential this package itself materialized (defense in depth on top
	// of whatever the journal's own scrubber does on commit).
	Data []byte
	// MediaType optionally categorizes Data.
	MediaType string
}

// ShellConfig configures one shell stage invocation.
type ShellConfig struct {
	// Command is the argv to execute (no shell interpolation).
	Command []string
	// Timeout overrides ShellExecutor.DefaultTimeout/package DefaultTimeout
	// when positive.
	Timeout time.Duration
	// MaxOutputBytes overrides ShellExecutor.DefaultMaxOutputBytes/package
	// DefaultMaxOutputBytes when positive.
	MaxOutputBytes int64
	// ResultFile, if set, is a path relative to the workspace whose bytes
	// (once the command exits) become a produced artifact named "result".
	// Its presence is then also required for success.
	ResultFile string
}

// ConfigFromEnvelope builds a ShellConfig from run.Command/Image and the
// well-known Input* keys in env.Inputs, per this package's convention for
// carrying stage config DeterministicRun doesn't yet declare (see doc.go).
func ConfigFromEnvelope(env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (ShellConfig, error) {
	cfg := ShellConfig{Command: run.Command, ResultFile: stringInput(env, InputResultFile)}
	if s := stringInput(env, InputTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return ShellConfig{}, fmt.Errorf("executor: invalid %s input %q: %w", InputTimeout, s, err)
		}
		cfg.Timeout = d
	}
	if s := stringInput(env, InputMaxOutputBytes); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return ShellConfig{}, fmt.Errorf("executor: invalid %s input %q", InputMaxOutputBytes, s)
		}
		cfg.MaxOutputBytes = n
	}
	return cfg, nil
}

// ShellExecutor runs deterministic shell stages: a declared argv command in a
// caller-provided workspace directory (a worktree the caller already created
// and owns — this package never touches internal/worktree).
type ShellExecutor struct {
	// DefaultTimeout overrides the package DefaultTimeout when positive.
	DefaultTimeout time.Duration
	// DefaultMaxOutputBytes overrides the package DefaultMaxOutputBytes when
	// positive.
	DefaultMaxOutputBytes int64
}

// NewShellExecutor returns a ShellExecutor with package defaults.
func NewShellExecutor() *ShellExecutor { return &ShellExecutor{} }

// Run executes cfg.Command in workspace with a capability-scoped, non-ambient
// environment (resolved via tokens for each of capabilities), enforces
// cfg.Timeout by killing the whole process group, captures size-bounded and
// secret-scrubbed stdout/stderr as produced artifacts, and — if
// cfg.ResultFile is set — lifts that file into a produced artifact and
// requires its presence for success.
//
// A non-nil error means Run itself could not produce a result
// (misconfiguration or credential resolution failure) — ARCHITECTURE.md
// invariant 6, fail closed rather than degrade. Everything the *declared
// command* can go wrong in (nonzero exit, timeout, a missing declared result
// file) is a normal ResultFailure envelope for the caller's retry policy to
// act on. The returned ResultEnvelope.Artifacts is always empty — see
// ProducedArtifact.
func (e *ShellExecutor) Run(ctx context.Context, workspace string, capabilities []string, tokens TokenSource, cfg ShellConfig) (apiv1.ResultEnvelope, []ProducedArtifact, error) {
	if len(cfg.Command) == 0 {
		return apiv1.ResultEnvelope{}, nil, errors.New("executor: ShellConfig declares no command")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = e.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxOutput := cfg.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = e.DefaultMaxOutputBytes
	}
	if maxOutput <= 0 {
		maxOutput = DefaultMaxOutputBytes
	}

	registry, scrubber := journal.DefaultScrubber()
	stageEnv, err := buildStageEnv(ctx, tokens, capabilities, registry)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("executor: resolve credentials: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = workspace
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
			Summary: fmt.Sprintf("failed to start %q", cfg.Command[0]),
		}, nil, nil
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
		waitErr = <-waitDone
	}

	outBytes := scrubber.Scrub(stdout.buf.Bytes())
	errBytes := scrubber.Scrub(stderr.buf.Bytes())

	result := apiv1.ResultEnvelope{Outputs: map[string]interface{}{}, Metrics: map[string]float64{}}
	produced := []ProducedArtifact{
		{Name: "stdout.log", Data: outBytes, MediaType: "text/plain"},
		{Name: "stderr.log", Data: errBytes, MediaType: "text/plain"},
	}
	if stdout.truncated {
		result.Outputs["stdoutTruncated"] = true
	}
	if stderr.truncated {
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
		return result, produced, nil
	}

	exitCode := exitCodeOf(waitErr)
	result.Metrics["exitCode"] = float64(exitCode)

	if cfg.ResultFile != "" {
		data, rerr := os.ReadFile(filepath.Join(workspace, cfg.ResultFile))
		switch {
		case rerr == nil:
			produced = append(produced, ProducedArtifact{
				Name: "result", Data: scrubber.Scrub(data), MediaType: mediaTypeFor(cfg.ResultFile),
			})
		case os.IsNotExist(rerr):
			result.Status = apiv1.ResultFailure
			result.Error = &apiv1.ErrorInfo{
				Code:      "missing_result_file",
				Message:   fmt.Sprintf("declared result file %q was not produced", cfg.ResultFile),
				Retryable: false,
			}
			result.Summary = "declared result file missing"
			return result, produced, nil
		default:
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("executor: read result file %q: %w", cfg.ResultFile, rerr)
		}
	}

	if exitCode == 0 {
		result.Status = apiv1.ResultSuccess
		result.Summary = "stage completed"
		return result, produced, nil
	}
	result.Status = apiv1.ResultFailure
	result.Error = &apiv1.ErrorInfo{
		Code:      "nonzero_exit",
		Message:   fmt.Sprintf("command exited %d", exitCode),
		Retryable: false,
	}
	result.Summary = fmt.Sprintf("command exited %d", exitCode)
	return result, produced, nil
}

func stringInput(env apiv1.InvocationEnvelope, key string) string {
	v, ok := env.Inputs[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
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

func mediaTypeFor(path string) string {
	if strings.HasSuffix(path, ".json") {
		return "application/json"
	}
	return "application/octet-stream"
}

// capturingWriter caps total bytes retained from a stream at limit, silently
// discarding (but still acknowledging, so the writer never blocks or errors
// the producing process) anything beyond it.
type capturingWriter struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func (w *capturingWriter) Write(p []byte) (int, error) {
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
