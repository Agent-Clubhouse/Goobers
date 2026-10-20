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

// Native pod ci-poll reuses the local CIPollExecutor, routed repository stamp,
// and stage credential plane. No instance config or provider login is mounted.
//
// Accepted substrate difference (decision 005 C5): pods cannot consult or update
// the daemon's in-memory ProviderQuotaState. Rate-limited failures report the
// reset through Outputs for the daemon's observer, but successful response quota
// observations do not reach that shared state. A pod may therefore spend one
// request beyond a window the daemon already knows is exhausted. Closing that
// gap requires a quota plane, not another dispatch payload.

// A missing capability is a workflow declaration fault, not a broker failure.
const ciPollCapabilityUndeclaredCode = "capability_not_declared"

// Gitea's forge URL is not carried to pods; unknown providers remain refused.
// Never fall back to a same-named GitHub repository.
const ciPollProviderUnsupportedCode = "ci_poll_provider_unsupported"

// runCIPollStage surrenders the same result and failure codes as the local
// executor. Infrastructure failures cross the pod boundary as Error.Retryable.
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
	cfg, err := podCIPollConfig(declared)
	if err != nil {
		return failureEnvelope("stage_declaration_invalid", err.Error())
	}
	// Resolution happens HERE, at stage start, against the daemon's credential
	// plane — never at dispatch — so no secret ever rides a dispatch payload or
	// a pod spec, and the pod receives only the capabilities its stage declared.
	creds, repoScheme, err := resolveStageCredentialsWithScheme(ctx)
	if err != nil {
		return failureEnvelope("credential_resolve_failed", err.Error())
	}
	token := mintedCredentialValue(creds, required)
	if token == "" {
		return failureEnvelope("credential_resolve_failed", fmt.Sprintf(
			"the credential plane returned no value for declared capability %q", required,
		))
	}
	// Register every resolved token BEFORE anything can carry it: the ci-checks
	// evidence artifact, and every failure message below, pass through this
	// scrubber. The local path gets the same protection from the run's
	// registrar (the executor registers each materialized token with the
	// journal scrubber before the stage runs). A value re-resolved after a 401
	// is registered by the refreshing source before it is used.
	registry, scrubber := journal.DefaultScrubber()
	for _, cred := range creds {
		registry.Register([]byte(cred.Value))
	}
	scrub := func(s string) string { return string(scrubber.Scrub([]byte(s))) }

	var adoSource providers.ADOCredentialSource
	if os.Getenv(executor.RepoProviderEnvVar) == string(providers.ProviderADO) {
		adoSource, err = podCIPollADOCredentialSource(creds, repoScheme, required, registry)
		if err != nil {
			return failureEnvelope("credential_resolve_failed", scrub(err.Error()))
		}
	}
	poller, err := podCIPollPoller(token, podCIPollTokenSource(creds, required, registry), adoSource)
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

	// This is the pod backstop, not InvocationEnvelope.Limits: the stamped
	// default must not stretch the local executor's undeclared 30m poll window.
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
			Retryable: providers.IsTransientError(err) || executor.IsCIPollInfrastructureError(err),
		},
	}
	if code == executor.CIPollNoVerdictCode {
		envelope.Summary = "ci-poll: CI finished without a verdict (#6817)"
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
		executor.InputNoVerdict,
		executor.InputCarryOutputs,
		executor.InputRetryFailedChecksMaxAttempts,
		executor.InputRetryFailedChecksBackoffSeconds,
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

// podCIPollConfig refuses inputs whose pod transport cannot preserve semantics.
func podCIPollConfig(declared []string) (executor.CIPollConfig, error) {
	env := podCIPollEnvelope(declared)
	if _, present := env.Inputs[executor.InputCarryOutputs]; present {
		return executor.CIPollConfig{}, fmt.Errorf("ci-poll carryOutputs is unsupported in pods; use a self runner to preserve typed input values")
	}
	cfg, err := executor.CIPollConfigFromEnvelope(env)
	if err == nil && cfg.RetryFailedChecksMaxAttempts > 0 {
		return executor.CIPollConfig{}, fmt.Errorf("ci-poll failed-check reruns are unsupported in pods; use a provider-supported self runner")
	}
	return cfg, err
}

// podCIPollPoller builds the PR poller for the routed repository.
//
// It shares the newPRPoller seam with the in-process path
// (runnerwiring_executors.go) on purpose: a parity test that substitutes one
// fake must be able to drive BOTH substrates through it, or the test proves
// only that two different fakes behave differently.
func podCIPollPoller(token string, source providers.TokenSource, adoSource providers.ADOCredentialSource) (executor.PRPoller, error) {
	if strings.TrimSpace(os.Getenv(executor.RepoProviderEnvVar)) == "" {
		return nil, errors.New("ci-poll pod requires an explicit routed provider stamp; instance configuration fallback is forbidden")
	}
	repo, err := providerRepo(providerStageRoot(""))
	if err != nil {
		return nil, fmt.Errorf("resolve ci-poll repository: %w", err)
	}
	if repo.Provider == providers.ProviderADO {
		if adoSource == nil {
			return nil, errors.New("ADO ci-poll pod requires an explicit broker credential source; ambient authentication fallback is forbidden")
		}
		provider, err := newProviderForStageAs[*providers.ADOProvider](providerStageRoot(""), repo, true,
			withStageProviderCapability(capability.ProviderPRWrite), withStageProviderADOCredential(adoSource))
		if err == nil {
			providers.WithADOPollScope(repo)(provider)
		}
		return provider, err
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
		withStageProviderToken(token), withStageProviderTokenSource(source),
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
