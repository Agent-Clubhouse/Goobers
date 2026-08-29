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
	"github.com/goobers/goobers/internal/procenv"
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

	// Carry whatever this stage committed to the next one (#3763). This pod is
	// about to be disposed, so a commit that does not leave here does not exist
	// downstream — on the worker the shared branch ref does this for free.
	//
	// Only for a stage that SUCCEEDED: a failed stage's half-finished commits
	// are not a base for the next stage to build on, and the engine retries it
	// on a fresh pod from the last good delta.
	//
	// A publish failure converts the stage to a FAILURE. The commits exist and
	// nothing else will carry them, so surrendering success here would strand
	// exactly the diff this mechanism protects — the silent shape #3763 is about.
	var delta publishedWorkspaceDelta
	if envelope.Status == apiv1.ResultSuccess {
		published, derr := publishWorkspaceDelta(ctx, ".", stderr)
		if derr != nil {
			pf(stderr, "dispatch-exec: %v\n", derr)
			envelope = apiv1.ResultEnvelope{
				Status:  apiv1.ResultFailure,
				Summary: "stage committed work that could not be carried to the next stage",
				Error:   &apiv1.ErrorInfo{Code: "workspace_delta_failed", Message: derr.Error()},
			}
		} else {
			delta = published
		}
	}
	data, err := json.Marshal(dispatcher.SurrenderedResult{
		Result: envelope, WorkspaceDelta: delta.Digest, WorkspaceDeltaBase: delta.Base, WorkspaceDeltaTip: delta.Tip,
		WorkspaceDeltaUnchanged: delta.Unchanged,
	})
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
	// An AGENTIC stage has no declared command: it executes by invoking a
	// goober through its harness, using the kit the dispatcher published. The
	// kit digest is what distinguishes the two, and it is stamped only for
	// agentic attempts.
	if strings.TrimSpace(os.Getenv(dispatcher.EnvAgenticKitDigest)) != "" {
		return runAgenticStage(ctx, stdout, stderr)
	}

	// THE INVARIANT THAT MAKES THE REST OF THIS FUNCTION SAFE, stated because
	// it is currently enforced by an ABSENCE and an absence is invisible to the
	// next change: only the agentic branch above materializes this stage's
	// ContextPointers (dispatchcontext.go). A deterministic stage does not need
	// it because internal/executor never resolves them — `grep -rn
	// ContextPointers internal/executor/ internal/dispatcher/` returns nothing,
	// so a declared command's inputs reach it as env and argv, not as journal
	// paths. IF A DETERMINISTIC KIND EVER STARTS CONSUMING ContextPointers, it
	// inherits #3823's half-B bug exactly — an unresolvable pointer against a
	// staging root nothing filled — and the fix is to call materializePodContext
	// here too, against the root that consumer reads.

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

	// Decision 003 ruling 3, pod-entrypoint backstop: the engine's
	// dispatchRemoteTask already refuses a stage that needs the daemon's
	// instance root before ever creating a pod (a ledger-touching or
	// journal-reading goobers CLI command, or a built-in stage kind with no
	// pod-side execution path). This re-asserts the identical refusal HERE,
	// at the one point in the tree where every substrate skew — an older
	// engine image dispatching to a newer worker, a hand-built attempt —
	// would actually surface, rather than trusting that the workflow-side
	// check happened. Gated on GOOBERS_INSTANCE_ROOT being unset (always
	// true in a pod today, since the dispatcher never stamps it) rather than
	// "this is a pod": once a plane client lands and a pod gets a scoped
	// root, this stops firing on its own, no dispatchexec.go change needed.
	if executor.StageRequiresInstanceRoot(argv, os.Getenv(dispatcher.InputEnvVar(executor.InputKind))) &&
		strings.TrimSpace(os.Getenv(executor.InstanceRootEnvVar)) == "" {
		return failureEnvelope(executor.StageRequiresInstanceRootCode, fmt.Sprintf(
			"stage command %v requires the daemon's instance root (%s is unset in this pod); this should have been refused before dispatch",
			argv, executor.InstanceRootEnvVar,
		))
	}

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

	// Provision the declared workspace BEFORE the stage runs. A repo workspace
	// checks the repository out at the run branch; scratch is a no-op, so every
	// stage that ran before pod-side checkout existed behaves identically.
	//
	// Ordered after credential resolution because the checkout authenticates
	// with a credential the stage already declared — no separate
	// workspace-provisioning credential path exists, deliberately.
	//
	// The working directory IS the workspace (podspec stamps WorkingDir), so
	// checking out into "." is what puts the stage's command inside the tree.
	// The checkout may use a credential the stage itself never receives.
	checkoutCreds, checkoutErr := resolveCheckoutCredential(ctx)
	if checkoutErr != nil {
		return failureEnvelope("credential_resolve_failed", checkoutErr.Error())
	}
	if err := checkoutRepoWorkspace(ctx, ".", stderr, append(append([]dispatcher.MintedCredential{}, creds...), checkoutCreds...)); err != nil {
		// Fail closed and NAME the workspace: a stage whose repo never arrived
		// would otherwise run against an empty directory and fail somewhere far
		// away — a missing Makefile, a missing test file — with an error that
		// says nothing about provisioning.
		return failureEnvelope("workspace_provision_failed", err.Error())
	}
	// The STAGE's git needs the same exemption: it runs in the same
	// differently-owned workspace, and real workflows commit and push from it.
	if mode := strings.TrimSpace(os.Getenv(dispatcher.EnvStageWorkspace)); mode != "" && mode != string(apiv1.WorkspaceScratch) {
		extraEnv = append(extraEnv, workspaceGitEnv(".").Env()...)
	}
	// Built from the STAGE's credentials only. checkoutCreds is deliberately
	// absent: it exists to provision the working tree, and exporting it here
	// would hand repository authority to a stage that never declared it —
	// the over-grant #3770 exists to avoid.
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
	// ORDER IS A SECURITY PROPERTY, not a style choice (#3725). Only
	// stageEnvironment() — the INHERITED container environment — is subject to
	// the env:default-deny allowlist. credEnv (the credentials this stage's own
	// declared capabilities just resolved to) and extraEnv (the git exemption,
	// the implicit resultFile) are appended AFTER it and never travel through
	// it.
	//
	// Collapsing this into one build-a-map-then-filter-once step is the natural
	// refactor and is exactly the bug #3725 was filed to prevent: procenv's
	// allowlist has no knowledge of GOOBERS_CRED_*, so a single filter strips
	// the credential the stage just resolved. The failure lands at the PROVIDER
	// — providerToken() reads empty, the CLI makes an unauthenticated call,
	// GitHub answers 401/404 — on a restricted runner class and not on a plain
	// one, which is a long way from "the env allowlist dropped the credential".
	// Answering it by allowlisting GOOBERS_CRED_* BY NAME would be worse: it
	// would also admit any image-baked GOOBERS_CRED_* the isolation exists to
	// deny. Keep the append.
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
	//
	// The returned pointers go on the ENVELOPE too, and that is not
	// duplication. The journal emission and the envelope reach the run by
	// different routes: emissions land in the LIVE journal directly, while the
	// envelope is surrendered and replayed into Temporal history. A projection
	// rebuilt from history therefore cannot see an emission-only artifact, and
	// the two journals diverge — MEASURED on a live cluster, on a run that
	// reported completed/success:
	//   live_journal_divergence: normative event 4 diverges:
	//     live:      type=artifact.recorded name=cli-on-pod/stdout.log
	//     projected: type=stage.finished
	// Carrying them on the envelope also closes the parity gap that a
	// pod-executed stage surrendered zero artifacts where the same stage on a
	// self runner surrendered three.
	// The exit code is a MEASUREMENT of the run and belongs on every envelope
	// below, success or failure. The local executor records it unconditionally
	// (shell.go: result.Metrics["exitCode"]), so a gate or report reading it
	// found a number on a self runner and nothing at all on a pod.
	exitCode := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		exitCode = exitErr.ExitCode()
	case runErr != nil:
		exitCode = -1
	}
	stageMetrics := map[string]float64{"exitCode": float64(exitCode)}

	stageArtifacts := recordStageArtifacts(ctx, stderr, map[string][]byte{
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
				Status:    apiv1.ResultFailure,
				Outputs:   outputs,
				Artifacts: stageArtifacts,
				Metrics:   stageMetrics,
				Summary:   "declared result file missing",
				Error: &apiv1.ErrorInfo{
					Code:    "missing_result_file",
					Message: fmt.Sprintf("stage declared result file %q and exited 0 without writing it", resultFile),
				},
			}
		case rerr != nil && !os.IsNotExist(rerr):
			return apiv1.ResultEnvelope{
				Status:    apiv1.ResultFailure,
				Outputs:   outputs,
				Artifacts: stageArtifacts,
				Metrics:   stageMetrics,
				Summary:   "declared result file unreadable",
				Error:     &apiv1.ErrorInfo{Code: "result_file_unreadable", Message: fmt.Sprintf("read result file %q: %v", resultFile, rerr)},
			}
		}
	}

	if runErr == nil && !timedOut {
		// noWork is a DISTINCT terminal status, not a flavour of success, and
		// the local executor has always treated it that way. A pod that
		// flattened it to success made a stage's "nothing to do" invisible —
		// the workflow would take the did-work path on a pod and the no-work
		// path on a self runner, from the same stage and the same outputs.
		if v, ok := outputs[executor.OutputNoWork].(bool); ok && v {
			return apiv1.ResultEnvelope{
				Status:    apiv1.ResultNoWork,
				Outputs:   outputs,
				Artifacts: stageArtifacts,
				Metrics:   stageMetrics,
				Summary:   "stage found no work to do",
			}
		}
		return apiv1.ResultEnvelope{
			Status:    apiv1.ResultSuccess,
			Outputs:   outputs,
			Artifacts: stageArtifacts,
			Metrics:   stageMetrics,
			Summary:   "stage completed",
		}
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
	// A FAILED stage carries its logs too — this is the case where they matter
	// most. The local executor attaches them on failure as well, and a pod that
	// surrendered artifacts only on success would make a failing stage harder
	// to diagnose on the substrate that is already harder to reach into.
	// A TYPED error reported through the result file beats the generic
	// stage_failed: the command knew why it failed and said so structurally.
	// The local executor gives it precedence for exactly that reason, so
	// without this the same failure journals as a specific code (e.g.
	// github_rate_limited, with the reset time) on a self runner and as an
	// opaque stage_failed on a pod — the classification a retry policy reads.
	if typedCode, typedMessage, retryable := consumeErrorOutputs(outputs); typedCode != "" {
		if typedMessage == "" {
			typedMessage = message
		}
		return apiv1.ResultEnvelope{
			Status:    apiv1.ResultFailure,
			Outputs:   outputs,
			Artifacts: stageArtifacts,
			Metrics:   stageMetrics,
			Summary:   typedMessage,
			Error:     &apiv1.ErrorInfo{Code: typedCode, Message: typedMessage, Retryable: retryable},
		}
	}
	return apiv1.ResultEnvelope{
		Status:    apiv1.ResultFailure,
		Outputs:   outputs,
		Artifacts: stageArtifacts,
		Metrics:   stageMetrics,
		Summary:   message,
		Error:     &apiv1.ErrorInfo{Code: code, Message: message},
	}
}

// consumeErrorOutputs reads the well-known typed-failure keys a command sets in
// its declared result file and REMOVES them from Outputs, mirroring the local
// executor's helper of the same name (internal/executor/shell.go).
//
// Deliberately mirrored rather than shared: this package cannot reach the
// executor's unexported helper, and the KEYS are the contract — they are
// exported constants, used here, so the two cannot drift on the names. The
// removal matters as much as the read: leaving errorCode in Outputs would hand
// downstream stages a key the self runner consumed, which is itself a parity
// gap.
func consumeErrorOutputs(outputs map[string]interface{}) (code, message string, retryable bool) {
	code, _ = outputs[executor.OutputErrorCode].(string)
	message, _ = outputs[executor.OutputErrorMessage].(string)
	retryable, _ = outputs[executor.OutputErrorRetryable].(bool)
	delete(outputs, executor.OutputErrorCode)
	delete(outputs, executor.OutputErrorMessage)
	delete(outputs, executor.OutputErrorRetryable)
	return code, message, retryable
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
//
// It builds ONLY the inherited-container-environment half. The stage's resolved
// credentials and the executor's own extras are appended by the caller AFTER
// this returns and never pass through it — see the call site.
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
	// UNLESS the runner class enforces env:default-deny (#3725), in which case
	// inheriting the image's ambient environment is exactly what the class
	// promises not to do, and the inherited half is rebuilt from procenv's
	// allowlist instead — the same list the local executor composes from.
	inherited := os.Environ()
	if os.Getenv(dispatcher.EnvStageEnvDefaultDeny) == "true" {
		inherited = procenv.BaseEnvWith(dispatcherStampedEnvNames())
	}
	// A goobers-CLI stage keeps its run identity; every other stage is stripped
	// of the whole control plane. The privileged half — above all the pod token
	// — is removed either way, so this widens what a CLI stage can READ, never
	// what any stage can DO. See DispatcherRunIdentityEnv for why the split.
	//
	// It runs AFTER the default-deny rebuild, not before, and that order is
	// load-bearing: the dispatcher-stamped allowlist re-admits variables by
	// name, so a stage whose declared `env:` named a control variable would
	// otherwise have re-admitted it into its own environment.
	stripped := dispatcher.DispatcherControlEnv
	if os.Getenv(dispatcher.EnvStageIsCLI) == "true" {
		stripped = dispatcher.DispatcherPrivilegedEnv
	}
	control := make(map[string]struct{}, len(stripped))
	for _, name := range stripped {
		control[name] = struct{}{}
	}
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

// dispatcherStampedEnvNames decodes the names the DISPATCHER stamped on this
// stage's behalf (GOOBERS_STAGE_ENV_ALLOW): its declared `env:` keys, its
// GOOBERS_INPUT_* inputs, its run identity and run context, any env a DI-9
// template declared on the stage container, and the instance's declared
// envPassthrough.
//
// The run-identity half is in that list and MUST be, even though it is control
// plane: the rebuild below runs BEFORE the CLI/non-CLI strip, so a name missing
// here is gone before the split can decide to keep it. A goobers-CLI stage
// keeps GOOBERS_RUN_ID/GAGGLE/WORKFLOW/STAGE/ATTEMPT by design — without them
// providerRunContext() fails closed and providers.BranchName cannot compose the
// run branch — while a non-CLI stage loses them again at the strip.
//
// They need naming because in a pod they arrive as ordinary container
// variables — at os.Environ() a stage's own declared `FOO=bar` is
// indistinguishable from one the image exported — so procenv's allowlist alone
// would drop the stage's declared environment and its inputs along with the
// image's ambient vars.
//
// A missing or unparseable list yields nothing, which fails CLOSED: the stage
// runs on procenv's built-in allowlist alone rather than on os.Environ().
func dispatcherStampedEnvNames() []string {
	encoded := strings.TrimSpace(os.Getenv(dispatcher.EnvStageEnvAllow))
	if encoded == "" {
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(encoded), &names); err != nil {
		return nil
	}
	return names
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
	return resolveCapabilities(ctx, capabilities)
}

// resolveCheckoutCredential mints the credential that provisions a repo
// workspace, when the dispatcher named one (#3770).
//
// SEPARATE from the stage's credentials on purpose: the result authenticates
// the clone inside this process and is never added to credEnv, so a stage does
// not gain repository authority merely by needing a working tree. Returns nil
// when the stage already declares a repo-shaped capability — the checkout uses
// that one, and minting a second would be pointless.
func resolveCheckoutCredential(ctx context.Context) ([]dispatcher.MintedCredential, error) {
	capability := strings.TrimSpace(os.Getenv(dispatcher.EnvCheckoutCapability))
	if capability == "" {
		return nil, nil
	}
	return resolveCapabilities(ctx, []string{capability})
}

func resolveCapabilities(ctx context.Context, capabilities []string) ([]dispatcher.MintedCredential, error) {
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
// recordStageArtifacts emits the stage's streams as artifacts through the
// daemon's journal plane and RETURNS the pointers it emitted, so the caller can
// carry them on the surrendered envelope as well.
//
// The pointers are derived, not read back: artifact storage is content
// addressed, so journal.ArtifactRef computes the exact Ref the daemon's writer
// will produce from the same bytes, with no extra round trip and no dependence
// on the emit response (which returns sequence counters, not refs). This is
// sound only because the bytes handed in are ALREADY SCRUBBED — RecordArtifact
// scrubs before digesting, so an unscrubbed input here would silently address a
// different blob than the one the daemon stores.
func recordStageArtifacts(ctx context.Context, stderr io.Writer, streams map[string][]byte) []apiv1.ArtifactPointer {
	daemonAPI := strings.TrimSpace(os.Getenv(dispatcher.EnvDaemonAPI))
	runID := os.Getenv(dispatcher.EnvRunID)
	stage := os.Getenv(dispatcher.EnvStage)
	if daemonAPI == "" || runID == "" || stage == "" {
		return nil
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
	// nil when this pod has no blob endpoint (the pre-blob-plane deployment
	// shape); one client for the whole batch.
	blobs := podBlobClient()
	putCtx, cancelPut := stageBlobWriteThroughContext(ctx, blobs)
	defer cancelPut()
	ops := make([]livejournal.Op, 0, len(names))
	pointers := make([]apiv1.ArtifactPointer, 0, len(names))
	var putFailures []string
	for _, name := range names {
		data := streams[name]
		if len(data) == 0 {
			continue
		}
		ref, refErr := journal.ArtifactRef(data)
		if refErr != nil {
			// Emit the artifact regardless: a pointer we cannot derive is a
			// missing envelope entry, not a reason to drop the journal record.
			_, _ = fmt.Fprintf(stderr, "goobers: derive artifact ref for %s: %v\n", name, refErr)
		} else {
			pointers = append(pointers, apiv1.ArtifactPointer{
				Path:      ref.Path,
				Digest:    ref.Digest,
				Size:      ref.Size,
				MediaType: "text/plain",
				Integrity: apiv1.IntegrityDerived,
			})
			if putErr := putStageArtifactBlob(putCtx, stderr, blobs, name, ref.Digest, data); putErr != nil {
				putFailures = append(putFailures, fmt.Sprintf("%s (%s, %d bytes): %v", name, ref.Digest, len(data), putErr))
			}
		}
		ops = append(ops, livejournal.Op{
			Kind: livejournal.OpArtifact,
			Key:  stage + "/" + name,
			// See podArtifactRecorder.Append (dispatchagentic.go): the daemon's
			// replayClock adopts this field verbatim, so an unstamped op here
			// durably persists the artifact's op at 0001-01-01T00:00:00Z (#3774).
			Time: time.Now().UTC(),
			Artifact: &livejournal.ArtifactOp{
				Stage:   stage,
				Attempt: attempt,
				Name:    stage + "/" + name,
				Data:    data,
			},
		})
	}
	// A DROPPED WRITE-THROUGH MUST LEAVE A DURABLE RECORD, on the producing
	// stage, in the plane that is still up.
	//
	// Without this the only signal is the stderr line above, and that stream is
	// the POD PROCESS's own — not the captured stderr.log artifact, which is
	// the stage command's — so it reaches no journal and dies with the pod at
	// disposal. The operator's first symptom would then be a DIFFERENT stage on
	// a DIFFERENT pod refusing with errContextBlobMissing and naming a digest
	// whose producer is already gone. Half B's fail-closed makes that a hard
	// stop rather than a degraded run, which is exactly why the evidence has to
	// outlive the pod that has it.
	//
	// It rides the journal plane precisely BECAUSE the blob plane is the thing
	// that just failed; the two are different endpoints, and the journal one
	// must be up regardless or the stage cannot surrender at all. Emitted as an
	// artifact rather than a stage failure: the stage's own result is still
	// authoritative (the write-through is best-effort by design), and this is a
	// measurement of the data plane, not a business outcome.
	if len(putFailures) > 0 {
		sort.Strings(putFailures)
		body := "stage " + stage + " attempt " + strconv.Itoa(attempt) +
			": the blob plane did not accept these artifacts, so a downstream stage that needs them will refuse with \"upstream context artifact is not in the blob plane\"\n" +
			strings.Join(putFailures, "\n") + "\n"
		ops = append(ops, livejournal.Op{
			Kind: livejournal.OpArtifact,
			Key:  stage + "/" + blobWriteThroughFailureArtifact,
			Artifact: &livejournal.ArtifactOp{
				Stage:   stage,
				Attempt: attempt,
				Name:    stage + "/" + blobWriteThroughFailureArtifact,
				Data:    []byte(body),
			},
		})
	}
	if len(ops) == 0 {
		return nil
	}
	emitter := &livejournal.HTTPEmitter{BaseURL: daemonAPI, Token: os.Getenv(dispatcher.EnvPodToken)}
	if _, err := emitter.Emit(ctx, livejournal.EmitRequest{
		RunID:  runID,
		Gaggle: os.Getenv(dispatcher.EnvGaggle),
		Ops:    ops,
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "dispatch-exec: record stage artifacts: %v\n", err)
	}
	return pointers
}

// putStageArtifactBlob write-throughs ONE pod-produced artifact to the blob
// plane under the digest its pointer was derived from (#3823, half A).
//
// This is the pod's half of the distributed data plane's WRITE side, and until
// it existed there was no such half. A pod-produced artifact reached the run
// through the journal plane alone (the OpArtifact above), which stores it in the
// daemon's journal — not in the blob store. The fleet's FETCH side
// (workerhost.MaterializeContext) reads the blob store by digest, so an artifact
// a pod produced was unreachable to every later stage that ran anywhere else:
// the pointer named a digest the store had never been told about. A
// worker-executed stage has had this write-through since the beginning
// (internal/workerhost/artifacts.go StagingArtifacts.record, which Puts to the
// same store), so this closes an asymmetry rather than inventing a mechanism.
//
// Same digest, same bytes, by construction: the caller hands in the exact slice
// journal.ArtifactRef derived the ref from (already scrubbed), so what the
// pointer names and what the store holds cannot diverge.
//
// BEST EFFORT, matching the journal emit beside it: the stage has already
// produced its result, and a blob plane that is down must not convert a
// completed stage into a failure. The failure it does cause is on the READ side
// — a later stage that needs this artifact fails closed with a named error
// (materializePodContext) rather than running without it — which is the right
// place for it, because that stage is the one that actually needs the bytes.
func putStageArtifactBlob(ctx context.Context, stderr io.Writer, blobs *dispatcher.BlobClient, name, digest string, data []byte) error {
	if blobs == nil || digest == "" || len(data) == 0 {
		return nil
	}
	if err := blobs.Put(ctx, digest, data); err != nil {
		_, _ = fmt.Fprintf(stderr, "dispatch-exec: publish artifact %s (%s, %d bytes) to the blob plane: %v\n", name, digest, len(data), err)
		return err
	}
	return nil
}

// blobWriteThroughFailureArtifact is the name recordStageArtifacts journals a
// dropped write-through under. Named as a constant because it is the string an
// operator greps for and the string the test asserts on.
const blobWriteThroughFailureArtifact = "blob-write-through.errors"

// blobWriteThroughBudget bounds the WHOLE batch of blob PUTs a single
// recordStageArtifacts call makes.
//
// ONE BUDGET FOR THE BATCH, not one per artifact, because the per-artifact
// default is dispatcher.BlobClient's 60s (internal/dispatcher/blob.go) and the
// batch is not bounded to two: on the agentic path every span routes through
// RecordSpanWithSchema -> RecordArtifact -> recordStageArtifacts. Serial 60s
// stalls after the stage has already finished are how a pod that has done its
// work fails to surrender for minutes and gets reclaimed by the disposal gate
// as an infrastructure fault. A REFUSING endpoint is instant (connection
// refused); a DROPPING one — the NetworkPolicy shape this change's own evidence
// plan tells operators to look for — is what needs the ceiling. Generous for
// the payload (stream artifacts are capped at 32 KiB by boundedCapture, and a
// span transcript by DefaultMaxTranscriptBytes) and far below anything that
// would look like a hung pod.
//
// A var, not a const, so the CEILING ITSELF is testable in bounded time (#3805):
// a hanging plane is the one failure mode this budget exists for, and a test
// that had to wait the real 15s to observe it would never be written.
var blobWriteThroughBudget = 15 * time.Second

// stageBlobWriteThroughContext returns the context the write-through PUTs run
// under, and it is DELIBERATELY NOT THE STAGE'S.
//
// The caller's ctx is the pod process's signal context (runDispatchExec ->
// signals.SetupSignalContext). On SIGTERM — pod deletion, eviction, node drain,
// the disposal gate — that context is already cancelled, so every Put would
// fail instantly with "context canceled" while the derived pointers still ride
// the surrendered envelope: the pointer names a digest the blob store was never
// told about, which is verbatim the half-A defect this change exists to remove.
// The surrender PUT two frames up already takes this position for the same
// reason ("the stage's own timeout must not also truncate the PUT that reports
// its outcome"), and the agentic path is already immune because
// podArtifactRecorder.RecordArtifact passes context.Background().
//
// context.WithoutCancel keeps the caller's values while dropping its
// cancellation, so a deadline of our own is the only thing that can stop these
// PUTs.
func stageBlobWriteThroughContext(ctx context.Context, blobs *dispatcher.BlobClient) (context.Context, context.CancelFunc) {
	if blobs == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), blobWriteThroughBudget)
}
