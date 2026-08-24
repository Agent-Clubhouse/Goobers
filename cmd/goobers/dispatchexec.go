package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/signals"
)

// dispatchexec.go is the mode-3 in-pod stage runtime (#3699): the process a
// dispatcher-rendered pod's Command/Args actually invokes
// (internal/dispatcher/podspec.go stamps ["goobers", "__dispatch-exec"] onto
// every stage container). It reads the pinned DeterministicRun and identity
// facts from the pod's environment (the same GOOBERS_* contract every stage
// pod already carries, plus the GOOBERS_STAGE_* additions podspec.go
// stamps), runs the stage exactly as a plain command/script, and PUTs a
// SurrenderedResult to the daemon write API's surrender plane
// (internal/dispatcher/surrenderclient.go) before exiting — the write the
// disposal gate (goobernetes-architecture.md §5) has been waiting for.
//
// v1 scope: the same guards internal/engine/dispatchstage.go's dispatch-time
// refusal enforces (workspace: scratch, no capabilities, no goobers-CLI/
// provider-builtin command) mean this process never needs a credential
// injector, journal writer, or repo checkout — it runs the command, captures
// bounded output, and surrenders. It exits 0 once the surrender PUT
// succeeds, REGARDLESS of the stage's own success/failure: the surrendered
// ResultEnvelope.Status is what the engine reads, not this process's exit
// code (the same principle the local executor's ResultFailure envelope
// already follows). It exits nonzero only when it cannot surrender at all —
// which the existing gate already treats as an infrastructure fault and
// retries on a fresh pod, so no changes are needed on the reader side.

// dispatchExecMaxCapturedOutput bounds how much of the command's stdout/
// stderr each is inlined into the surrendered ResultEnvelope.Outputs — a
// diagnostic excerpt, not artifact-grade capture (v1 has no journal wired
// into the pod to record a full artifact against). Comfortably under the
// surrender plane's 1 MiB body cap (internal/httpapi/surrenderplane.go) even
// with both streams present.
const dispatchExecMaxCapturedOutput = 32 << 10

func runDispatchExec(_ []string, stdout, stderr io.Writer) int {
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	return runDispatchExecContext(ctx, stdout, stderr)
}

func runDispatchExecContext(ctx context.Context, stdout, stderr io.Writer) int {
	runID := os.Getenv(dispatcher.EnvRunID)
	stage := os.Getenv(dispatcher.EnvStage)
	daemonAPI := os.Getenv(dispatcher.EnvDaemonAPI)
	podToken := os.Getenv(dispatcher.EnvPodToken)
	attempt, attemptErr := strconv.Atoi(os.Getenv(dispatcher.EnvAttempt))

	// Nothing to surrender to: fail loud immediately rather than run the
	// stage for nothing. The disposal gate already treats "pod terminated,
	// no surrendered result" as an infrastructure fault and retries on a
	// fresh pod — this is that path, just diagnosed clearly in the pod's own
	// logs instead of silently.
	if daemonAPI == "" || podToken == "" || attemptErr != nil {
		pf(stderr, "dispatch-exec: missing or invalid pod identity (%s/%s/%s/%s); cannot surrender a result\n",
			dispatcher.EnvRunID, dispatcher.EnvStage, dispatcher.EnvAttempt, dispatcher.EnvDaemonAPI)
		return 1
	}

	client := &dispatcher.SurrenderPutClient{BaseURL: daemonAPI, Token: podToken}
	envelope := runDeclaredStage(ctx, stdout, stderr)
	data, err := json.Marshal(dispatcher.SurrenderedResult{Result: envelope})
	if err != nil {
		pf(stderr, "dispatch-exec: marshal surrendered result: %v\n", err)
		return 1
	}
	// Surrender rides its own background context: the stage's own timeout
	// (already applied inside runDeclaredStage) must not also truncate the
	// PUT that reports its outcome.
	if err := client.Put(context.Background(), runID, stage, attempt, data); err != nil {
		pf(stderr, "dispatch-exec: surrender result: %v\n", err)
		return 1
	}
	return 0
}

// runDeclaredStage builds and runs the pinned Command/Script, and always
// returns a ResultEnvelope — success, failure, or an infra-shaped failure
// for a malformed declaration — never an error, because the caller's only
// job past this point is to surrender whatever envelope comes back.
func runDeclaredStage(ctx context.Context, stdout, stderr io.Writer) apiv1.ResultEnvelope {
	run := apiv1.DeterministicRun{Script: os.Getenv(dispatcher.EnvStageScript)}
	if encoded := os.Getenv(dispatcher.EnvStageCommand); encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &run.Command); err != nil {
			return failureEnvelope("stage_declaration_invalid", fmt.Sprintf("decode %s: %v", dispatcher.EnvStageCommand, err))
		}
	}
	argv, extraEnv, cleanup, err := executor.DeterministicCommand(run)
	if err != nil {
		return failureEnvelope("stage_declaration_invalid", err.Error())
	}
	defer cleanup()

	timeout := dispatcher.DefaultStageTimeout
	if declared, err := time.ParseDuration(os.Getenv(dispatcher.EnvStageTimeout)); err == nil && declared > 0 {
		timeout = declared
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), extraEnv...)
	capturedStdout := &boundedCapture{limit: dispatchExecMaxCapturedOutput}
	capturedStderr := &boundedCapture{limit: dispatchExecMaxCapturedOutput}
	cmd.Stdout = io.MultiWriter(stdout, capturedStdout)
	cmd.Stderr = io.MultiWriter(stderr, capturedStderr)

	runErr := cmd.Run()
	outputs := map[string]interface{}{}
	if capturedStdout.Len() > 0 {
		outputs["stdout"] = capturedStdout.String()
	}
	if capturedStderr.Len() > 0 {
		outputs["stderr"] = capturedStderr.String()
	}
	if runErr == nil {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: outputs}
	}

	code, message := "stage_failed", runErr.Error()
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		message = fmt.Sprintf("exit code %d: %s", exitErr.ExitCode(), capturedStderr.String())
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		code, message = "stage_timeout", fmt.Sprintf("stage exceeded its %s timeout", timeout)
	}
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Outputs: outputs,
		Error:   &apiv1.ErrorInfo{Code: code, Message: message},
	}
}

func failureEnvelope(code, message string) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: code, Message: message}}
}

// boundedCapture is an io.Writer that keeps at most limit bytes, silently
// discarding the rest — a diagnostic excerpt, not a lossless capture (see
// dispatchExecMaxCapturedOutput).
type boundedCapture struct {
	buf   []byte
	limit int
}

func (b *boundedCapture) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *boundedCapture) Len() int { return len(b.buf) }

func (b *boundedCapture) String() string { return string(b.buf) }
