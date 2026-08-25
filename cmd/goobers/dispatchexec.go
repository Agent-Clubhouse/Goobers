package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/platform/proc"
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

	cmd := exec.Command(argv[0], argv[1:]...)
	// Resolve this stage's declared credentials against the daemon's credential
	// plane and inject them exactly as the local executor does. Resolution
	// happens HERE, at stage start, not at dispatch — so no secret ever rides
	// a dispatch payload or a pod spec (DS9/DS10, #2931).
	creds, credErr := resolveStageCredentials(ctx)
	if credErr != nil {
		// Fail closed. A stage that declared capabilities and did not get them
		// would run uncredentialed and fail far away, against the provider,
		// with an error naming the provider rather than the credential plane.
		return failureEnvelope("credential_resolve_failed", credErr.Error())
	}
	credEnv := make([]string, 0, len(creds))
	for _, cred := range creds {
		credEnv = append(credEnv, capability.CredentialEnvVar(cred.Capability)+"="+cred.Value)
	}
	// A provider builtin writes its result to an IMPLICIT path when the stage
	// declared no resultFile — the local executor derives it from the
	// subcommand (shell.go), and so must the pod, or the builtin writes a file
	// nobody reads and the stage reports empty outputs. Derived from the same
	// exported helper, not a second table.
	resultFile := os.Getenv(dispatcher.InputEnvVar("resultFile"))
	if resultFile == "" && len(argv) > 1 && executor.StageInvokesGoobersCLI(argv) {
		if implicit, ok := executor.ProviderStageResultFile(argv[1]); ok {
			resultFile = implicit
			// The child reads the path from this variable, exactly as it does
			// on a self runner. cmd.Dir is unset and the container's WorkingDir
			// IS the workspace, so a workspace-relative path resolves the same
			// on both substrates.
			extraEnv = append(extraEnv, dispatcher.InputEnvVar("resultFile")+"="+implicit)
		}
	}
	cmd.Env = append(append(stageEnvironment(), credEnv...), extraEnv...)
	capturedStdout := &boundedCapture{limit: dispatchExecMaxCapturedOutput}
	capturedStderr := &boundedCapture{limit: dispatchExecMaxCapturedOutput}
	cmd.Stdout = io.MultiWriter(stdout, capturedStdout)
	cmd.Stderr = io.MultiWriter(stderr, capturedStderr)

	// proc.Start + a tree-kill on timeout (not exec.CommandContext, whose
	// context-cancel path only reaches the direct child): the declared
	// command commonly runs through an intermediate shell ("sh -c ..."), and
	// only a whole-process-tree kill reliably reclaims a timed-out stage
	// regardless of how many processes it spawned or whether the shell
	// happened to exec-replace itself for a trivial script. Mirrors
	// internal/executor/shell.go's exact pattern, minus its SIGQUIT
	// diagnostics dump — a v1 in-pod stage has no journal to record a
	// goroutine-trace artifact against.
	tree, startErr := proc.Start(cmd)
	if startErr != nil {
		return failureEnvelope("exec_start", startErr.Error())
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var runErr error
	timedOut := false
	select {
	case runErr = <-waitDone:
	case <-runCtx.Done():
		// Fires for either the declared timeout or the outer signal-context
		// being canceled (SIGTERM to the pod) — both cases mean the stage
		// must not be reported as having quietly succeeded.
		timedOut = true
		_ = tree.Kill()
		runErr = <-waitDone
	}

	// Scrub resolved credentials out of anything the stage wrote. The local
	// executor registers every token with a scrubber before the stage runs;
	// without this a stage that echoes its token surrenders it into the
	// journal, where it is durable and widely readable.
	registry, scrubber := journal.DefaultScrubber()
	for _, cred := range creds {
		registry.Register([]byte(cred.Value))
	}
	outputs := map[string]interface{}{}
	scrubbedOut := scrubber.Scrub([]byte(capturedStdout.String()))
	scrubbedErr := scrubber.Scrub([]byte(capturedStderr.String()))
	if capturedStdout.Len() > 0 {
		outputs["stdout"] = string(scrubbedOut)
	}
	if capturedStderr.Len() > 0 {
		outputs["stderr"] = string(scrubbedErr)
	}
	// Record the stage's streams as artifacts through the daemon's journal
	// plane, as the local executor does. Best-effort BY DESIGN: the stage has
	// already run, and its ResultEnvelope is the authoritative outcome — losing
	// a log artifact must not turn a completed stage into a failure. Failures
	// are surfaced on stderr so they are visible rather than silent.
	recordStageArtifacts(ctx, stderr, map[string][]byte{
		"stdout.log": scrubbedOut,
		"stderr.log": scrubbedErr,
	})
	// Lift the declared result file into Outputs, exactly as the local
	// executor does. WITHOUT THIS a pod-executed stage surrenders only stdout,
	// so a gate reading an output key finds nothing and evaluates its FAILURE
	// branch — measured on a live cluster: a stage emitted {"verdict":"pass"},
	// the gate read no `verdict` key, and the run took the fail path three
	// times before exhausting its repass budget. The run "completed" with the
	// wrong control flow and nothing reported an error.
	if resultFile != "" {
		data, rerr := os.ReadFile(resultFile)
		switch {
		case rerr == nil:
			mergeResultFileOutputs(outputs, data)
		case os.IsNotExist(rerr) && runErr == nil && !timedOut:
			// A stage that succeeded but did not write its declared result
			// file is a FAILURE, same as locally: the declaration is a
			// contract, and honouring the exit code alone would report a
			// success whose outputs the workflow cannot read.
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultFailure,
				Outputs: outputs,
				Error: &apiv1.ErrorInfo{
					Code:    "missing_result_file",
					Message: fmt.Sprintf("stage declared result file %q and exited 0 without writing it", resultFile),
				},
			}
		case rerr != nil && !os.IsNotExist(rerr):
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultFailure,
				Outputs: outputs,
				Error:   &apiv1.ErrorInfo{Code: "result_file_unreadable", Message: fmt.Sprintf("read result file %q: %v", resultFile, rerr)},
			}
		}
	}

	if runErr == nil && !timedOut {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: outputs}
	}

	code, message := "stage_failed", "stage exited with an error"
	if runErr != nil {
		message = runErr.Error()
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			message = fmt.Sprintf("exit code %d: %s", exitErr.ExitCode(), capturedStderr.String())
		}
	}
	if timedOut {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			code, message = "stage_timeout", fmt.Sprintf("stage exceeded its %s timeout", timeout)
		} else {
			code, message = "stage_interrupted", "dispatch-exec was interrupted before the stage finished"
		}
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

// mergeResultFileOutputs merges a stage's declared result file into Outputs.
// Mirrors executor.mergeResultFileOutputs deliberately, INCLUDING its rules:
// invalid JSON is ignored rather than failing the stage, and only scalar
// values are lifted. Any divergence here reappears as a gate that evaluates
// differently depending on which substrate ran the stage.
func mergeResultFileOutputs(outputs map[string]interface{}, data []byte) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return
	}
	for key, value := range parsed {
		switch value.(type) {
		case string, float64, bool:
			outputs[key] = value
		}
	}
}

// stageEnvironment builds the environment a stage command actually receives.
//
// It does NOT inherit the pod's environment. The local executor composes a
// stage environment from procenv's default-deny allowlist plus declared values;
// a pod inheriting os.Environ() diverges from that AND hands the stage the
// dispatcher's own control plane — including GOOBERS_POD_TOKEN, the credential
// that authorizes surrendering this run's results. A stage able to read it can
// author its own outcome.
//
// Kept: the procenv allowlist (the same one the local executor uses) and the
// stage's declared inputs, which a stage is meant to read.
// Dropped: every dispatcher control variable.
func stageEnvironment() []string {
	// The pod's environment is the stage's environment: podspec stamps the
	// stage's declared Env as NATIVE container variables, and the runner image
	// provides its own (PLAYWRIGHT_BROWSERS_PATH, GOOBERS_TEMPORAL_CLI, …).
	// Both must reach the command — dropping image-provided variables is the
	// documented failure where "the browsers were present and INVISIBLE".
	//
	// What must NOT reach it is the dispatcher's own control plane, above all
	// GOOBERS_POD_TOKEN: it authorizes surrendering THIS run's results, so a
	// stage able to read it can author its own outcome — report success for
	// work that failed. MEASURED before this filter, inside a real pod:
	// POD_TOKEN=PRESENT in a 24-variable inherited environment.
	//
	// NOTE, deliberately not fixed here: this is NOT procenv's default-deny
	// allowlist, so a runner declaring env:default-deny still does not get it.
	// True parity needs the instance's envPassthrough threaded into the pod,
	// because in-pod the allowlisted values come from the IMAGE rather than
	// from the daemon's environment. That is a design change, not a filter.
	// A goobers-CLI stage keeps its run identity; every other stage is stripped
	// of the whole control plane. The privileged half — above all the pod token
	// — is removed either way, so this widens what a CLI stage can READ, never
	// what any stage can DO. See DispatcherRunIdentityEnv for why the split.
	stripped := dispatcher.DispatcherControlEnv
	if os.Getenv(dispatcher.EnvStageIsCLI) == "true" {
		stripped = dispatcher.DispatcherPrivilegedEnv
	}
	control := make(map[string]struct{}, len(stripped))
	for _, name := range stripped {
		control[name] = struct{}{}
	}
	inherited := os.Environ()
	env := make([]string, 0, len(inherited))
	for _, kv := range inherited {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, isControl := control[name]; isControl {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// resolveStageCredentials asks the daemon's credential plane for the
// capabilities this stage declared. The dispatcher stamps the capability NAMES
// only; the values never exist outside this process and the daemon.
func resolveStageCredentials(ctx context.Context) ([]dispatcher.MintedCredential, error) {
	encoded := strings.TrimSpace(os.Getenv(dispatcher.EnvStageCapabilities))
	if encoded == "" {
		return nil, nil
	}
	var capabilities []string
	if err := json.Unmarshal([]byte(encoded), &capabilities); err != nil {
		return nil, fmt.Errorf("decode %s: %w", dispatcher.EnvStageCapabilities, err)
	}
	if len(capabilities) == 0 {
		return nil, nil
	}
	daemonAPI := strings.TrimSpace(os.Getenv(dispatcher.EnvDaemonAPI))
	if daemonAPI == "" {
		return nil, fmt.Errorf("stage declares capabilities %v but %s is unset; the pod cannot reach the credential plane", capabilities, dispatcher.EnvDaemonAPI)
	}
	client := &dispatcher.CredentialResolveClient{
		BaseURL: daemonAPI,
		Token:   os.Getenv(dispatcher.EnvPodToken),
	}
	return client.Resolve(ctx, os.Getenv(dispatcher.EnvRunID), os.Getenv(dispatcher.EnvStage), capabilities)
}

// recordStageArtifacts writes the stage's streams into the run journal through
// the daemon's journal plane. The pod has no local journal — the plane is the
// only path — and this is what makes a pod-executed stage's output inspectable
// after the pod is disposed, which is the whole point of a fresh-per-attempt
// substrate.
func recordStageArtifacts(ctx context.Context, stderr io.Writer, streams map[string][]byte) {
	daemonAPI := strings.TrimSpace(os.Getenv(dispatcher.EnvDaemonAPI))
	runID := os.Getenv(dispatcher.EnvRunID)
	stage := os.Getenv(dispatcher.EnvStage)
	if daemonAPI == "" || runID == "" || stage == "" {
		return
	}
	attempt, err := strconv.Atoi(os.Getenv(dispatcher.EnvAttempt))
	if err != nil || attempt < 1 {
		attempt = 1
	}
	names := make([]string, 0, len(streams))
	for name := range streams {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic op order; the daemon assigns sequence
	ops := make([]livejournal.Op, 0, len(names))
	for _, name := range names {
		data := streams[name]
		if len(data) == 0 {
			continue
		}
		ops = append(ops, livejournal.Op{
			Kind: livejournal.OpArtifact,
			Key:  stage + "/" + name,
			Artifact: &livejournal.ArtifactOp{
				Stage:   stage,
				Attempt: attempt,
				Name:    stage + "/" + name,
				Data:    data,
			},
		})
	}
	if len(ops) == 0 {
		return
	}
	emitter := &livejournal.HTTPEmitter{BaseURL: daemonAPI, Token: os.Getenv(dispatcher.EnvPodToken)}
	if _, err := emitter.Emit(ctx, livejournal.EmitRequest{
		RunID:  runID,
		Gaggle: os.Getenv(dispatcher.EnvGaggle),
		Ops:    ops,
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "dispatch-exec: record stage artifacts: %v\n", err)
	}
}
