package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// dispatchcipoll.go is decision 005 step C5 (#3881): `kind: ci-poll` executed
// IN THE POD, in-process, by dispatch-exec.
//
// WHY IT IS A BRANCH AND NOT A COMMAND. ci-poll has no CLI subcommand — the
// workflow's `command: ["goobers", "ci-poll"]` is a placeholder that exists
// only because DeterministicRun.Command is a required schema field, and the
// stage is selected by inputs.kind (internal/executor's TaskExecutor). On a
// self runner the in-process CIPollExecutor serves it; in a pod, until this
// file, nothing did, so the kind was refused before dispatch
// (executor.StageRequiresInstanceRoot's kind arm) and the whole
// `implementation` lane was pinned to the daemon's instance root. Running the
// SAME executor here — not a new poller, not a re-implementation — is what
// keeps a pod ci-poll and a self ci-poll one behaviour with one set of
// outputs.
//
// WHAT IT NEEDS AND WHAT IT DOES NOT. ci-poll touches no claim ledger, no
// merge lock and no on-disk run journal; it needs exactly one thing the pod
// cannot fabricate — a PR-provider token — and the pod already has the route
// for that: the daemon's credential plane, resolved at stage start from the
// capability NAMES the dispatcher stamped (dispatchexec.go's
// resolveStageCredentials, DS9/DS10). So `external-telemetry` stays refused
// (its executor is built from the instance's connector configuration, which
// lives under a config directory a pod has none of) and only `ci-poll` leaves
// the refusal list.
//
// ---------------------------------------------------------------------------
// NAMED, ACCEPTED DEGRADATION: THE PRE-DISPATCH QUOTA CONSULT IS LOST.
// ---------------------------------------------------------------------------
//
// On a self runner, buildCIPollExecutor (runnerwiring_executors.go) builds
// ci-poll's provider WITH a providers.QuotaObserver backed by the scheduler's
// localscheduler.ProviderQuotaState. That observer does TWO things:
//
//  1. AcquireQuotaRequest, BEFORE each provider request — the daemon declines
//     to spend a request it knows is beyond an exhausted window; and
//  2. ObserveQuota, AFTER each response — every response's rate-limit headers
//     update the shared state, including on SUCCESS.
//
// A POD HAS NEITHER. ProviderQuotaState is daemon-process memory; a pod cannot
// read it before polling and cannot write to it after. Half (2) is recovered,
// deliberately and only for the rate-limited case, by REPORTING: a
// rate-limited poll surrenders providers.ErrorCodeRateLimited with
// executor.OutputRateLimitReset in Outputs, which is exactly the pair the
// daemon's live RateLimited observer (#3876, D1) consumes to call
// ProviderQuotaState.RecordExhausted — the same pair every provider-backed CLI
// stage already surrenders (cmd/goobers/providercmd.go's classifyProviderError)
// and the same pair internal/runner's taskOutcome already reads.
//
// Half (1) IS LOST, and this is the statement that it is lost on purpose
// rather than by oversight (finding 002, "Plan corrections adopted", C5):
//
//   - a pod ci-poll CANNOT decline to poll because the window is already
//     exhausted; it will make the request, be rejected, and report the
//     rejection. The daemon learns the reset one round trip LATER than it
//     would locally, and burns one request doing so.
//   - quota observed on SUCCESSFUL pod polls does not reach the daemon at all.
//     ProviderQuotaState therefore sees a pod-executed lane's consumption only
//     through the rate-limited failures it eventually causes, not through the
//     headers that predicted them.
//
// Both shrink the scheduler's warning window; neither can produce a wrong
// RESULT — a rate-limited poll is a retryable failure on both substrates, with
// the same code, and the reset lands in the same state object one tick later.
// Closing the gap properly means a quota plane the pod can consult (the
// increment-2 shape, alongside the claims plane), not a wider dispatch payload.

// ciPollCapabilityUndeclaredCode names a ci-poll stage that reached a pod
// without declaring provider:pr:write. It is NOT credential_resolve_failed:
// nothing was asked of the credential plane and nothing failed there — the
// WORKFLOW is missing a declaration, and an operator reading
// credential_resolve_failed would go looking at the daemon's resolver for a
// fault that is in a YAML file. (v_2_0's compiler already rejects this shape
// statically; this is the runtime backstop for a hand-built or
// version-skewed attempt, in the same spirit as the instance-root backstop.)
const ciPollCapabilityUndeclaredCode = "capability_not_declared"

// ciPollProviderUnsupportedCode names a pod ci-poll routed to a provider the
// POD cannot construct a poller for. Azure DevOps and Gitea both resolve part
// of their identity from the instance config directory — the ADO provider
// shells out to `az` against config-declared auth, and Gitea's forge BaseURL
// is deliberately not carried on the wire (providercmd.go's
// validateRoutedRepo) — and a stage pod has no config directory. Refused
// loudly HERE rather than silently defaulting to GitHub, which would poll a
// real api.github.com repository that merely shares the routed owner/name.
const ciPollProviderUnsupportedCode = "ci_poll_provider_unsupported"

// runCIPollStage executes the ci-poll kind in-process in this pod and always
// returns a ResultEnvelope, never an error: the caller's only remaining job is
// to surrender whatever envelope comes back (the same contract
// runDeclaredStage follows).
//
// The local path returns retryable failures as ERRORS across the invoke seam
// (internal/executor's ciPollKindExecutor wraps them in
// invoke.InfrastructureFailure). A pod has no such seam — the surrender plane
// carries an envelope — so a retryable failure is expressed as
// ErrorInfo.Retryable, exactly as dispatch-exec already expresses the
// base-sync conflict (#813). The CODE is identical on both sides by
// construction: both call executor.CIPollFailureCode.
func runCIPollStage(ctx context.Context, stderr io.Writer) apiv1.ResultEnvelope {
	declared, err := stageDeclaredCapabilities()
	if err != nil {
		return failureEnvelope("stage_declaration_invalid", err.Error())
	}
	required := string(capability.ProviderPRWrite)
	if !containsCapability(declared, required) {
		return failureEnvelope(ciPollCapabilityUndeclaredCode, fmt.Sprintf(
			"stage declares inputs.kind=%s but not capability %q; ci-poll polls a pull request through the PR provider and cannot run without it",
			executor.KindCIPoll, required,
		))
	}
	// Resolution happens HERE, at stage start, against the daemon's credential
	// plane — never at dispatch — so no secret ever rides a dispatch payload or
	// a pod spec, and the pod receives only the capabilities its stage declared.
	creds, err := resolveStageCredentials(ctx)
	if err != nil {
		return failureEnvelope("credential_resolve_failed", err.Error())
	}
	token := mintedCredentialValue(creds, required)
	if token == "" {
		return failureEnvelope("credential_resolve_failed", fmt.Sprintf(
			"the credential plane returned no value for declared capability %q", required,
		))
	}
	// Register the resolved token BEFORE anything can carry it: the ci-checks
	// evidence artifact, and every failure message below, pass through this
	// scrubber. The local path gets the same protection from the run's
	// registrar (the executor registers each materialized token with the
	// journal scrubber before the stage runs).
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte(token))
	scrub := func(s string) string { return string(scrubber.Scrub([]byte(s))) }

	poller, err := podCIPollPoller(token)
	if err != nil {
		return failureEnvelope(ciPollProviderUnsupportedCode, scrub(err.Error()))
	}
	ciPoll, err := executor.NewCIPollExecutor(poller, podArtifactRecorder{
		stderr:   stderr,
		scrubber: scrubber,
		dir:      podCIPollStagingDir(),
	})
	if err != nil {
		return failureEnvelope("stage_declaration_invalid", scrub(err.Error()))
	}
	cfg, err := executor.CIPollConfigFromEnvelope(podCIPollEnvelope(declared))
	if err != nil {
		// A malformed or missing prNumber/cadence input is a DECLARATION
		// problem, not a provider one: coding it as poll_provider_error would
		// send an operator to the provider's status page for a typo in
		// inputsFrom.
		return failureEnvelope("stage_declaration_invalid", scrub(err.Error()))
	}

	// The pod's own wall-clock budget, from the same source the shell path
	// reads (dispatchexec.go), so a poll cannot outlive the pod that is
	// supervising it. It is a BACKSTOP, not the poll window: the poll window
	// comes from the stage's declared pollTimeoutSeconds (or the executor's
	// 30m default) exactly as it does locally, and is shorter than this in
	// every shipped workflow.
	//
	// Deliberately NOT surfaced as InvocationEnvelope.Limits: the dispatcher
	// stamps GOOBERS_STAGE_TIMEOUT unconditionally, defaulting to
	// dispatcher.DefaultStageTimeout when the stage declared no limits, so a
	// pod cannot tell a DECLARED one-hour budget from an undeclared one.
	// Feeding that default into Limits would silently stretch an undeclared
	// ci-poll's window from the local 30m to an hour — a divergence on exactly
	// the lane this change exists to move.
	runCtx, cancel := context.WithTimeout(ctx, podStageTimeout())
	defer cancel()

	result, err := ciPoll.Run(runCtx, cfg)
	if err == nil {
		return result
	}
	return podCIPollFailure(runCtx, ctx, err, scrub)
}

// podCIPollFailure projects a CIPollExecutor error into the envelope this pod
// surrenders. runCtx is the poll's own (deadline-bearing) context and parent
// is dispatch-exec's; the two are separated for the same reason the executor
// separates them — an expired pod budget is a timeout, a cancelled parent is
// a SIGTERM, and they are different operator problems.
func podCIPollFailure(runCtx, parent context.Context, err error, scrub func(string) string) apiv1.ResultEnvelope {
	if parent.Err() != nil {
		return apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Summary: "ci-poll was interrupted before it reached a terminal check state",
			Error: &apiv1.ErrorInfo{
				Code:      "stage_interrupted",
				Message:   scrub(err.Error()),
				Retryable: true,
			},
		}
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Summary: "ci-poll exceeded the stage's wall-clock budget",
			Error: &apiv1.ErrorInfo{
				Code:      "stage_timeout",
				Message:   scrub(err.Error()),
				Retryable: true,
			},
		}
	}

	code := executor.CIPollFailureCode(err)
	envelope := apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Summary: "ci-poll provider request failed",
		Error: &apiv1.ErrorInfo{
			Code:    code,
			Message: scrub(err.Error()),
			// Matches the local path's split exactly: internal/executor's
			// ciPollKindExecutor wraps a transient error for infrastructure
			// retry and returns a terminal one as a plain ResultFailure.
			Retryable: providers.IsTransientError(err),
		},
	}
	if code == providers.ErrorCodeRateLimited {
		envelope.Summary = "ci-poll was rejected by the provider's rate limit"
		// THE HALF OF THE IN-PROCESS QUOTA OBSERVER A POD CAN STILL DELIVER.
		// The reset rides Outputs, not the error message, because that is the
		// key the daemon's RateLimited observer parses (internal/runner's
		// outputRateLimitReset) on its way to
		// ProviderQuotaState.RecordExhausted. Absent when the provider sent no
		// reset header — the consumers treat a missing reset as "nothing
		// actionable" and skip, which is better than a zero timestamp that
		// would park the scheduler at the epoch.
		if reset, ok := executor.CIPollRateLimitReset(err); ok {
			envelope.Outputs = map[string]interface{}{executor.OutputRateLimitReset: reset}
		}
	}
	return envelope
}

// podCIPollEnvelope rebuilds the slice of the InvocationEnvelope
// CIPollConfigFromEnvelope reads, from the environment the dispatcher stamped.
//
// An EXPLICIT list of input keys, not a sweep of every GOOBERS_INPUT_* in the
// environment: executor.InputEnvVar upper-cases and folds punctuation
// ("prNumber" -> GOOBERS_INPUT_PRNUMBER), so the mapping does not invert — a
// sweep would have to guess the original spelling of every key and would
// silently mis-name any it guessed wrong.
func podCIPollEnvelope(declared []string) apiv1.InvocationEnvelope {
	inputs := map[string]interface{}{}
	for _, key := range []string{
		executor.InputPROwner,
		executor.InputPRRepo,
		executor.InputPRNumber,
		executor.InputPollIntervalSec,
		executor.InputPollMaxIntervalSec,
		executor.InputPollTimeoutSec,
		executor.InputHumanPolicyIDs,
	} {
		if value := strings.TrimSpace(os.Getenv(dispatcher.InputEnvVar(key))); value != "" {
			inputs[key] = value
		}
	}
	return apiv1.InvocationEnvelope{
		Inputs:       inputs,
		Capabilities: declared,
		// prOwner/prRepo default from the run's routed repository when the
		// stage does not name them — the shipped implementation.yaml relies on
		// exactly that. GOOBERS_REPO_* is the dispatcher's stamp of
		// InvocationEnvelope.RepoRef (internal/engine's DispatchStage), which
		// is the same field CIPollConfigFromEnvelope falls back to locally.
		RepoRef: apiv1.RepoRef{
			Provider: apiv1.Provider(os.Getenv(executor.RepoProviderEnvVar)),
			Owner:    os.Getenv(executor.RepoOwnerEnvVar),
			Name:     os.Getenv(executor.RepoNameEnvVar),
		},
	}
}

// podCIPollPoller builds the PR poller for the routed repository.
//
// It shares the newPRPoller seam with the in-process path
// (runnerwiring_executors.go) on purpose: a parity test that substitutes one
// fake must be able to drive BOTH substrates through it, or the test proves
// only that two different fakes behave differently.
func podCIPollPoller(token string) (executor.PRPoller, error) {
	repo, err := providerRepo(providerStageRoot(""))
	if err != nil {
		return nil, fmt.Errorf("resolve ci-poll repository: %w", err)
	}
	if repo.Provider != providers.ProviderGitHub {
		return nil, fmt.Errorf(
			"ci-poll cannot run in a pod for repository provider %q: its poller is resolved from the instance config directory, which a stage pod does not have; place this stage on a self runner",
			repo.Provider,
		)
	}
	if newPRPoller != nil {
		return newPRPoller(token), nil
	}
	// Through the shared stage-provider seam: this runs IN a pod, which is
	// exactly where the configured bot login has to come from the dispatcher's
	// stamped run identity rather than an instance config that is not there
	// (#3914). The poller itself reads CI state, but the seam is what keeps
	// every in-pod GitHub construction identical.
	return newProviderForStageAs[*providers.GitHubProvider](providerStageRoot(""), repo, true,
		withStageProviderToken(token),
	)
}

// podStageTimeout is the effective wall-clock budget for this attempt, read
// from the same variable and with the same default the declared-command path
// uses, so one stage does not get two different budgets depending on whether
// it is a kind or a command.
func podStageTimeout() time.Duration {
	if declared, err := time.ParseDuration(os.Getenv(dispatcher.EnvStageTimeout)); err == nil && declared > 0 {
		return declared
	}
	return dispatcher.DefaultStageTimeout
}

// podCIPollStagingDir names the directory the artifact recorder reports from.
// ci-poll writes its evidence through the journal plane rather than the disk,
// so this is only the Dir() the recorder must be able to answer with; the
// working directory IS the pod's workspace (podspec stamps WorkingDir).
func podCIPollStagingDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func containsCapability(declared []string, want string) bool {
	for _, value := range declared {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func mintedCredentialValue(creds []dispatcher.MintedCredential, want string) string {
	for _, cred := range creds {
		if cred.Capability == want {
			return cred.Value
		}
	}
	return ""
}
