package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// Shared plumbing for the built-in provider-chain stage kinds (#131/#132):
// backlog-query, open-pr, issue-close-out. Unlike every other `goobers`
// subcommand, these are invoked by the runner as a deterministic stage's
// shell command — their process cwd is the stage's worktree, not the
// instance root — so they read their run context from GOOBERS_* env vars
// the runner injects (internal/executor/env.go's buildStageEnv), falling
// back to an explicit [path] argument for standalone/manual invocation.

// newGitHubProvider constructs the GitHub provider these subcommands talk to
// and projects rate-limit decisions into the stage telemetry sidecar. A package
// var (not a plain call to providers.NewGitHubProvider) so a
// CLI-level test can point it at an httptest.Server instead of the real
// api.github.com, mirroring runnerwiring.go's newPRPoller/repoCloneURL seams.
var newGitHubProvider = newTelemetryGitHubProvider

func newTelemetryGitHubProvider(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
	telemetryOpt := providers.WithRateLimitObserver(
		telemetry.NewStageRateLimitObserver(os.Getenv(telemetry.StageTelemetryEnv)),
	)
	provider := providers.NewGitHubProvider(token, append([]func(*providers.GitHubProvider){telemetryOpt}, opts...)...)
	if attribution, ok := stageAttribution(os.Getenv(executor.InstanceRootEnvVar)); ok {
		provider.SetAttribution(attribution)
	}
	return provider
}

// claimLedgerFileName/claimLockFileName are the well-known files under an
// instance's scheduler dir the claim ledger and its cross-process lock
// (withClaimLock) live at — shared by backlog-query (the claimant) and
// `goobers up`'s periodic RecoverExpired sweep (the reaper), both of which
// must agree on the same paths.
const (
	claimLedgerFileName = "claims.json"
	claimLockFileName   = "claims.lock"
	// mergeLockFileName is the cross-process file lock `merge-pr` (mergepr.go)
	// takes around its poll->decide->merge window (issue #719): once
	// merge-review's readiness allows several concurrent runs to review
	// DIFFERENT PRs at once, each independently reaches merge-pr, but only
	// one PR may be inside that window at a time — serializing it is what
	// lets a later run's re-poll (D6) actually observe an earlier run's
	// just-landed merge instead of racing it. A dedicated lock file, not
	// claimLockFileName, since this guards a different critical section
	// (the merge decision, not claim-ledger mutation) with a different
	// hold-time profile (network round-trips, not a fast local read/write).
	mergeLockFileName = "merge.lock"

	claimLockOperationBacklogFilterBlocked = "backlog-query.filter-blocked"
	claimLockOperationBacklogScanCursor    = "backlog-query.scan-cursor"
	claimLockOperationBacklogResweep       = "backlog-query.resweep"
	claimLockOperationBacklogReconcile     = "backlog-query.reconcile"
	claimLockOperationBacklogClaim         = "backlog-query.claim"
	claimLockOperationBacklogRelease       = "backlog-query.release"
	claimLockOperationSelectSourceClaim    = "select-source.claim"
	claimLockOperationSelectSourceRelease  = "select-source.release"
	claimLockOperationPRLookup             = "pr-claim.lookup"
	claimLockOperationPRAcquire            = "pr-claim.acquire"
	claimLockOperationPRRelease            = "pr-claim.release"
	claimLockOperationPRCount              = "pr-claim.count"
	// The two sections pr-select's FAIRNESS LEASE runs in
	// (stateclient.KeyPRSelectFairness, which rides claims.lock). The observe
	// label names the section the claim transaction already holds; the clear
	// label names the standalone rewrite that follows a successful selection.
	claimLockOperationPRSelectFairnessObserve = "pr-select.fairness-observe"
	claimLockOperationPRSelectFairnessClear   = "pr-select.fairness-clear"
	claimLockOperationPRSelectAdvisoryRecord  = "pr-select.advisory-record"
	claimLockOperationRunLookup               = "run-claims.lookup"
	claimLockOperationBlockedUpdate           = "blocked-records.update"
	claimLockOperationCircuitBreakerOutbox    = "circuit-breaker-outbox.update"
	claimLockOperationRecovery                = "claim-recovery"
	claimLockOperationRenewal                 = "claim-renewal"
	claimLockOperationRunRelease              = "run-claims.release"
	claimLockOperationCloseOutLookup          = "issue-close-out.lookup"
	claimLockOperationCloseOutRelease         = "issue-close-out.release"
	claimLockOperationMigration               = "claim-ledger.migrate"
	claimLockOperationAdminList               = "claims.list"
	claimLockOperationAdminRelease            = "claims.release"
	claimLockOperationIntervention            = "intervention.reacquire"

	claimLockSlowThreshold = 5 * time.Second
	claimLockRetryInterval = 10 * time.Millisecond
	claimsLockTimeoutCode  = "claims_lock_timeout"
)

type claimsLockTimeoutError struct {
	Operation    string
	Timeout      time.Duration
	WaitDuration time.Duration
}

func (e *claimsLockTimeoutError) Error() string {
	return fmt.Sprintf("claims lock operation %q timed out after %s", e.Operation, e.WaitDuration)
}

type journaledClaimsLockTimeoutError struct {
	timeout *claimsLockTimeoutError
}

func (e *journaledClaimsLockTimeoutError) Error() string { return e.timeout.Error() }
func (e *journaledClaimsLockTimeoutError) Unwrap() error { return e.timeout }

type claimLockEventContext struct {
	Gaggle   string
	Workflow string
	RunID    string
}

func isJournaledClaimsLockTimeout(err error) bool {
	var timeoutErr *journaledClaimsLockTimeoutError
	return errors.As(err, &timeoutErr)
}

// layoutFor is instance.NewLayout, named for readability at each provider-
// chain subcommand's call site.
func layoutFor(root string) instance.Layout {
	return instance.NewLayout(root)
}

// providerStageRoot resolves the instance root a provider-chain subcommand
// operates against: an explicit pathArg wins (standalone/manual invocation,
// matching every other subcommand's [path] convention), else
// GOOBERS_INSTANCE_ROOT (the runner-injected env var), else ".".
func providerStageRoot(pathArg string) string {
	if pathArg != "" {
		return pathArg
	}
	if root := os.Getenv(executor.InstanceRootEnvVar); root != "" {
		return root
	}
	return "."
}

func providerStageRootArg(fs *flag.FlagSet) (string, bool) {
	if fs.NArg() > 1 {
		fs.Usage()
		return "", false
	}
	return providerStageRoot(fs.Arg(0)), true
}

// providerRepo returns the repository routed into a stage invocation. Standalone
// commands without run context retain the original first-configured-repo
// fallback.
func providerRepo(root string) (providers.RepositoryRef, error) {
	routed := providers.RepositoryRef{
		Provider: providers.ProviderKind(os.Getenv(executor.RepoProviderEnvVar)),
		Owner:    os.Getenv(executor.RepoOwnerEnvVar),
		Project:  os.Getenv(executor.RepoProjectEnvVar),
		Name:     os.Getenv(executor.RepoNameEnvVar),
	}
	if routed.Provider != "" || routed.Owner != "" || routed.Project != "" || routed.Name != "" {
		if err := validateRoutedRepo(routed); err != nil {
			return providers.RepositoryRef{}, err
		}
		return routed, nil
	}
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	if len(cfg.Repos) == 0 {
		return providers.RepositoryRef{}, fmt.Errorf("no repo configured in %s", l.ConfigFile())
	}
	repo := cfg.Repos[0]
	switch providers.ProviderKind(repo.Provider) {
	case providers.ProviderGitHub:
		return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}, nil
	case providers.ProviderADO:
		return providers.RepositoryRef{Provider: providers.ProviderADO, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}, nil
	case providers.ProviderGitea:
		return providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: repo.Owner, Name: repo.Name, URL: repo.BaseURL}, nil
	default:
		return providers.RepositoryRef{}, fmt.Errorf("provider-chain command does not yet support repository provider %q", repo.Provider)
	}
}

// validateRoutedRepo enforces the per-provider identity a routed repository
// must carry: GitHub needs owner+name; Azure DevOps needs organization (owner),
// project, and repo name.
func validateRoutedRepo(routed providers.RepositoryRef) error {
	switch routed.Provider {
	case providers.ProviderGitHub:
		if routed.Owner == "" || routed.Name == "" {
			return fmt.Errorf("invalid routed github repository in %s/%s/%s", executor.RepoProviderEnvVar, executor.RepoOwnerEnvVar, executor.RepoNameEnvVar)
		}
		return nil
	case providers.ProviderADO:
		if routed.Owner == "" || routed.Project == "" || routed.Name == "" {
			return fmt.Errorf("invalid routed ado repository in %s/%s/%s/%s", executor.RepoProviderEnvVar, executor.RepoOwnerEnvVar, executor.RepoProjectEnvVar, executor.RepoNameEnvVar)
		}
		return nil
	case providers.ProviderGitea:
		// Gitea addresses a repo like GitHub (owner+name); BaseURL is resolved
		// from instance config by giteaRepoRefForStage, not carried on the wire.
		if routed.Owner == "" || routed.Name == "" {
			return fmt.Errorf("invalid routed gitea repository in %s/%s/%s", executor.RepoProviderEnvVar, executor.RepoOwnerEnvVar, executor.RepoNameEnvVar)
		}
		return nil
	default:
		return fmt.Errorf("invalid routed repository in %s/%s/%s", executor.RepoProviderEnvVar, executor.RepoOwnerEnvVar, executor.RepoNameEnvVar)
	}
}

// providerToken reads the capability-scoped credential the runner already
// resolved and injected for this stage process (executor/env.go's
// buildStageEnv, executor.CredentialEnvVar) — it never re-resolves the token
// independently, so it stays covered by the run's registrar-based secret
// scrubbing rather than becoming a second, unregistered copy of the secret.
func providerToken(cap capability.Capability) (string, error) {
	envVar := executor.CredentialEnvVar(string(cap))
	token := os.Getenv(envVar)
	if token == "" {
		return "", fmt.Errorf("no credential in %s env var — this subcommand must run as a stage declaring capabilities: [%q] (or set %s directly for standalone use)", envVar, cap, envVar)
	}
	return token, nil
}

// providerInput reads a declared Task.Inputs value the runner passed through
// as a GOOBERS_INPUT_* env var (executor/env.go's buildStageEnv,
// executor.InputEnvVar), falling back to def when unset.
func providerInput(key, def string) string {
	if v := os.Getenv(executor.InputEnvVar(key)); v != "" {
		return v
	}
	return def
}

// providerBranchNamespace resolves the run-branch namespace root the runner
// injects for this stage's gaggle (GOOBERS_BRANCH_NAMESPACE, set from
// GaggleSpec.BranchNamespace), falling back to providers.DefaultBranchNamespace
// when unset — standalone invocation, or a gaggle that keeps the default. It is
// the seam a PR-selector's headPrefix default and a run-branch head derivation
// both build on, so a gaggle that retunes its namespace selects, opens, and
// remediates PRs under the same prefix its branches use and the mirror-fetch
// exclusion preserves (#965/#1010). Always returns a value ending in "/".
func providerBranchNamespace() string {
	return providers.NormalizeBranchNamespace(os.Getenv(executor.BranchNamespaceEnvVar))
}

// providerBaseBranch resolves the gaggle's configured default branch the
// runner injects for this stage (GOOBERS_BASE_BRANCH, set from
// GaggleSpec.Project.Branch/RepoRef.Branch), falling back to "main" when
// unset — standalone invocation, or a gaggle that keeps the default. It is
// the seam every PR-lifecycle stage's "base" input default builds on, so a
// gaggle pinned to a non-"main" branch stops silently comparing/gating
// against "main" (#2087).
func providerBaseBranch() string {
	if b := os.Getenv(executor.BaseBranchEnvVar); b != "" {
		return b
	}
	return "main"
}

const providerCommandMargin = time.Second

// stageTimeout reports the wall-clock budget the shell executor is enforcing
// on this stage: its declared timeout input, or the executor default.
func stageTimeout() time.Duration {
	if s := providerInput(executor.InputTimeout, ""); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return executor.DefaultTimeout
}

// providerCommandBudget leaves time for a provider subcommand to report its
// result before the shell executor terminates the stage.
func providerCommandBudget(stage time.Duration) time.Duration {
	margin := providerCommandMargin
	if margin >= stage {
		margin = stage / 10
	}
	if budget := stage - margin; budget > 0 {
		return budget
	}
	return stage / 2
}

func providerCommandContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), providerCommandBudget(stageTimeout()))
}

// providerRunContext reads the run/workflow identity the runner injects for
// every stage process (GOOBERS_RUN_ID/GOOBERS_WORKFLOW). Both are required —
// a provider-chain subcommand run outside a real stage dispatch (e.g. by
// hand, with no GOOBERS_RUN_ID/GOOBERS_WORKFLOW set) has no meaningful
// claim/PR/branch identity to act under, so this fails closed rather than
// proceeding with an empty runID (which could collide with, or never match,
// a real run's claims) or an empty workflow (which would make
// providers.BranchName(workflow, runID) produce a malformed branch name).
func providerRunContext() (runID, workflow string, err error) {
	runID = os.Getenv(executor.RunIDEnvVar)
	workflow = os.Getenv(executor.WorkflowEnvVar)
	if runID == "" {
		return "", "", fmt.Errorf("GOOBERS_RUN_ID is not set — this subcommand must run as a workflow stage")
	}
	if workflow == "" {
		return "", "", fmt.Errorf("GOOBERS_WORKFLOW is not set — this subcommand must run as a workflow stage")
	}
	return runID, workflow, nil
}

func providerGaggle() string {
	return os.Getenv(executor.GaggleEnvVar)
}

// Typed error codes a provider-chain subcommand's declared result file
// carries via executor.OutputErrorCode (#711, generalizing #614's
// rate-limit-only classification to every provider failure shape). Each is
// checked in internal/executor/shell.go's OutputErrorCode convention, so a
// classified failure journals as itself instead of the maximally-
// uninformative missing_result_file it used to collapse into.
const (
	// errorCodeSecondaryRateLimited is a Retry-After-driven abuse/secondary
	// limit (providers.RateLimitError.Secondary), distinct from a primary
	// quota exhaustion (providers.ErrorCodeRateLimited): an operator (and
	// the retry policy) can tell "back off briefly, this token is being
	// abuse-throttled" from "wait for the hourly window to roll over".
	errorCodeSecondaryRateLimited = "github_secondary_rate_limited"
	// errorCodeServerError is a non-rate-limit 5xx / HTML error page —
	// GitHub server-side load shedding (the "Unicorn!" page seen live in
	// #705/#711), always retryable.
	errorCodeServerError = "github_server_error"
	// errorCodeAuthFailed is a 401, or a 403 that isn't itself a rate limit
	// (isRateLimited, providers/github_issues.go, already intercepts and
	// reports those as *RateLimitError before a plain status error can ever
	// see one) — a real permission failure. Never retryable: retrying with
	// the same bad or expired credential cannot succeed.
	errorCodeAuthFailed = providers.ErrorCodeAuthFailed
	// errorCodeNetwork is either a transport-level failure (dial/DNS/reset/
	// timeout) that exhausted send()'s own in-request retry budget, or any
	// other condition providers.IsTransientError recognizes without a
	// status code attached — retryable, since the failure is unrelated to
	// the request's content. A 429 never lands here: it classifies as a
	// rate limit ahead of this branch, whether typed or string-recovered
	// (#3647).
	errorCodeNetwork = "network_error"
	// errorCodeBranchMergeQueued is GitHub's transient GH006 rejection when
	// the branch being updated belongs to a pull request in the merge queue.
	errorCodeBranchMergeQueued = "github_branch_merge_queued"
	// errorCodeProvider is the fallback for a provider-originated failure
	// that doesn't classify into any of the above (e.g. a non-401/403/5xx
	// status such as a 422 validation error). Still typed and diagnosable —
	// never the raw missing_result_file — but not retried by default.
	errorCodeProvider = "provider_error"
)

// statusCodePattern extracts an HTTP status code from a provider error's
// message. providers' own non-2xx typed error (providerResponseError, #613)
// carries the status as a struct field, but that type is unexported —
// classifyProviderError, in a different package, can only recover it from
// the message text, the exact same "status %d" shape providers.
// IsTransientError's own cross-process-boundary fallback already relies on
// (providers/transient.go) for the identical reason.
var statusCodePattern = regexp.MustCompile(`status (\d{3})`)

func statusCodeFrom(err error) (int, bool) {
	m := statusCodePattern.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}
	code, convErr := strconv.Atoi(m[1])
	return code, convErr == nil
}

// classifyProviderError maps a provider-originated error into a stable
// errorCode, whether the stage retry budget should apply, and any
// classification-specific extra result-file fields (#711). Every
// failProviderStage caller passes an error that came directly from a
// providers.GitHubProvider call (see providercmd.go's call sites across
// cmd/goobers), so this always has a provider-shaped error to classify —
// there is no "not a provider error at all" case to additionally guard.
//
// The retryable verdict for the status-coded and residual branches
// deliberately never disagrees with providers.IsTransientError (#613) —
// this function only adds a finer-grained CODE on top of that same
// retryable/non-retryable split, never a second, independent opinion on
// whether the failure is retryable.
func classifyProviderError(err error) (code string, retryable bool, extra map[string]interface{}) {
	if rl, ok := providers.AsRateLimitError(err); ok {
		extra = map[string]interface{}{}
		if !rl.Reset.IsZero() {
			extra["rateLimitReset"] = rl.Reset.UTC().Format(time.RFC3339)
		}
		if rl.Secondary {
			return errorCodeSecondaryRateLimited, true, extra
		}
		return providers.ErrorCodeRateLimited, true, extra
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "gh006") && strings.Contains(message, "added to a merge queue") {
		return errorCodeBranchMergeQueued, true, nil
	}
	if providers.IsAuthenticationError(err) {
		return errorCodeAuthFailed, false, nil
	}
	if status, ok := statusCodeFrom(err); ok {
		switch {
		case status >= 500:
			return errorCodeServerError, true, nil
		case status == http.StatusTooManyRequests:
			// A 429 whose typed error did not survive a subprocess
			// boundary — only its message text did (#3647). Still a quota
			// failure, never the generic network_error it used to fall
			// through to; the reset metadata is unrecoverable from the
			// string, so no extra fields here.
			return providers.ErrorCodeRateLimited, true, nil
		}
	}
	if providers.IsTransientError(err) {
		return errorCodeNetwork, true, nil
	}
	// Last, so every provider-specific reading above still wins: a git
	// subprocess WE ran against a checkout, failing for a reason no provider
	// condition explains, is this process's own infrastructure and not
	// evidence about the item. provider_error was the fallback, and
	// telemetry.ClassifyError("provider_error") is not an infra fault, so the
	// #3361/#3364 circuit-breaker exemption never fired for one. See
	// gitCommandError (#4106).
	var gitErr *gitCommandError
	if errors.As(err, &gitErr) {
		return telemetry.ErrCodeInfraGit, false, nil
	}
	// Also last, for the same reason: an artifact an upstream stage of this
	// same run was supposed to leave behind is the SUBSTRATE's job to carry,
	// not the item's. RETRYABLE, unlike the git branch above, because on the
	// pod arm the producer emits the artifact op over the journal plane and a
	// different pod reads it back — "not applied yet" clears on its own, and
	// the stage retry budget is the instrument for it. See
	// upstreamArtifactError (#4121).
	var upstreamErr *upstreamArtifactError
	if errors.As(err, &upstreamErr) {
		return telemetry.ErrCodeInfraJournal, true, nil
	}
	return errorCodeProvider, false, nil
}

// runProviderStageCommand is the command-boundary result contract for provider
// stages. Handlers still write their domain outputs and typed provider errors;
// this exposes the resolved result path to those writers, then fills only a
// missing result while preserving the actual stderr reason from early business
// failures that returned before reaching them.
func runProviderStageCommand(name, resultFileDefault string, handler cliCommandHandler, args []string, stdout, stderr io.Writer) int {
	resultFile := providerInput("resultFile", resultFileDefault)
	if resultFile != "" {
		if err := os.Remove(resultFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			message := fmt.Sprintf("prepare provider-stage result %s: %v", resultFile, err)
			pf(stderr, "error: %s\n", message)
			errorCode, retryable, extra := classifyProviderError(errors.New(message))
			payload := map[string]interface{}{
				executor.OutputErrorCode:      errorCode,
				executor.OutputErrorMessage:   message,
				executor.OutputErrorRetryable: retryable,
			}
			for key, value := range extra {
				payload[key] = value
			}
			if writeErr := writeProviderStageResult(resultFile, payload); writeErr != nil {
				pf(stderr, "warning: write provider-stage result %s: %v\n", resultFile, writeErr)
			}
			return 1
		}
	}

	resultFileEnv := executor.InputEnvVar(executor.InputResultFile)
	originalResultFile, hadOriginalResultFile := os.LookupEnv(resultFileEnv)
	if resultFile != "" {
		if err := os.Setenv(resultFileEnv, resultFile); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("expose provider-stage result %s", resultFile), err, resultFile)
		}
		defer func() {
			if hadOriginalResultFile {
				_ = os.Setenv(resultFileEnv, originalResultFile)
			} else {
				_ = os.Unsetenv(resultFileEnv)
			}
		}()
	}

	var captured bytes.Buffer
	code := handler(args, stdout, io.MultiWriter(stderr, &captured))
	if code == 2 {
		return code
	}

	if resultFile == "" {
		return code
	}
	if validProviderStageResult(resultFile) {
		if err := ensureProviderStageResultIntegrity(resultFile); err != nil {
			pf(stderr, "error: finalize provider-stage result %s: %v\n", resultFile, err)
			return 1
		}
		return code
	}

	payload := map[string]interface{}{}
	if code != 0 {
		message := providerStageErrorMessage(name, captured.String())
		errorCode, retryable, extra := classifyProviderError(errors.New(message))
		payload = map[string]interface{}{
			executor.OutputErrorCode:      errorCode,
			executor.OutputErrorMessage:   message,
			executor.OutputErrorRetryable: retryable,
		}
		for key, value := range extra {
			payload[key] = value
		}
	}
	if err := writeProviderStageResult(resultFile, payload); err != nil {
		pf(stderr, "warning: write provider-stage result %s: %v\n", resultFile, err)
		return 1
	}
	return code
}

func validProviderStageResult(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && json.Valid(data)
}

func ensureProviderStageResultIntegrity(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var payload interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	changed, err := ensureProviderResultIntegrity(payload)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return writeProviderStagePayload(path, payload)
}

func ensureProviderResultIntegrity(payload interface{}) (bool, error) {
	switch payload := payload.(type) {
	case map[string]interface{}:
		return ensureProviderResultObjectIntegrity(payload)
	case []interface{}:
		changed := false
		for i, item := range payload {
			object, ok := item.(map[string]interface{})
			if !ok {
				return false, fmt.Errorf("result item %d must be an object", i)
			}
			itemChanged, err := ensureProviderResultObjectIntegrity(object)
			if err != nil {
				return false, fmt.Errorf("result item %d: %w", i, err)
			}
			changed = changed || itemChanged
		}
		return changed, nil
	default:
		return false, fmt.Errorf("result must be an object or array of objects")
	}
}

func ensureProviderResultObjectIntegrity(payload map[string]interface{}) (bool, error) {
	if raw, ok := payload["integrity"]; ok {
		grade, ok := raw.(string)
		if !ok || !apiintegrity.Grade(grade).Valid() {
			return false, fmt.Errorf("invalid integrity label %v", raw)
		}
		return false, nil
	}
	payload["integrity"] = string(apiintegrity.Unapproved)
	return true, nil
}

func providerStageErrorMessage(name, stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	fallback := ""
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if fallback == "" {
			fallback = line
		}
		if strings.HasPrefix(line, "error:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "error:"))
		}
	}
	if fallback != "" {
		return fallback
	}
	return name + " failed without an error message"
}

func writeProviderStageResult(path string, payload map[string]interface{}) error {
	if _, ok := payload["integrity"]; !ok {
		payload["integrity"] = string(apiintegrity.Unapproved)
	}
	return writeProviderStagePayload(path, payload)
}

func writeProviderStagePayload(path string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal typed result: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write typed result: %w", err)
	}
	return nil
}

// failProviderStage reports a stage-fatal provider error: it prints the same
// "error: <what>: <err>" line the call sites used to (unchanged operator UX)
// and returns 1. It also classifies err (#711, generalizing #614) and writes
// the declared result file with the structured errorCode fields
// internal/executor/shell.go lifts into the stage's ErrorInfo — so e.g. a
// quota-exhausted tick journals as github_rate_limited with the reset time
// in the message, and a 503/HTML load-shedding response journals as
// github_server_error, instead of both collapsing into the missing_result_file
// that used to bury the real cause under a raw-stderr archaeology exercise.
// resultFileDefault is the command's own default result-file name (the same
// value its success path passes to providerInput("resultFile", ...)); a
// command with no declared or default result file writes nothing and keeps
// plain nonzero_exit/missing_result_file semantics, since shell.go has
// nowhere fail-closed to read a structured signal from.
func failProviderStage(stderr io.Writer, what string, err error, resultFileDefault string) int {
	pf(stderr, "error: %s: %v\n", what, err)
	resultFile := providerInput("resultFile", resultFileDefault)
	if resultFile == "" {
		return 1
	}
	code, retryable, extra := classifyProviderError(err)
	payload := map[string]interface{}{
		executor.OutputErrorCode:      code,
		executor.OutputErrorMessage:   fmt.Sprintf("%s: %v", what, err),
		executor.OutputErrorRetryable: retryable,
	}
	for k, v := range extra {
		payload[k] = v
	}
	if werr := writeProviderStageResult(resultFile, payload); werr != nil {
		pf(stderr, "warning: write typed error result %s: %v\n", resultFile, werr)
	}
	return 1
}

// withClaimLock serializes fn against every other process (a concurrent
// `goobers backlog-query` from a racing run, or `goobers up`'s periodic
// RecoverExpired) touching the same instance's claim ledger, via a bounded
// exclusive file lock on lockPath. localscheduler.ClaimLedger's own mutex is
// documented as in-process only ("designed for one embedded scheduler per
// instance... not cross-process file locking") — but backlog-query runs as
// its own OS process per stage dispatch, so two concurrent runs'
// backlog-query subprocesses each open an independent ClaimLedger instance
// against the SAME claims.json file: without this lock, both could read the
// file before either persists, and the second write would silently clobber
// the first's claim (a lost update), defeating "one claim wins" (#131's
// acceptance criterion). The loser waits for a consistent read, but only up
// to runConditions.claimsLockTimeout so a wedged holder cannot convoy the
// runner indefinitely.
// Every caller supplies a stable operation label. Wait and hold durations are
// measured for every acquisition, while only threshold breaches are journaled
// so normal lock traffic stays low-noise.
func withClaimLock(lockPath, operation string, fn func() error) error {
	return withClaimLockThreshold(lockPath, operation, claimLockSlowThreshold, fn)
}

func withClaimLockThreshold(lockPath, operation string, slowThreshold time.Duration, fn func() error) error {
	timeout, err := claimsLockTimeoutForPath(lockPath)
	if err != nil {
		return err
	}
	return withClaimLockBounds(lockPath, operation, timeout, slowThreshold, claimLockEventContext{
		Gaggle:   providerGaggle(),
		Workflow: os.Getenv(executor.WorkflowEnvVar),
		RunID:    os.Getenv(executor.RunIDEnvVar),
	}, fn)
}

func withClaimLockForRun(lockPath, operation, gaggle, runID string, fn func() error) error {
	timeout, err := claimsLockTimeoutForPath(lockPath)
	if err != nil {
		return err
	}
	return withClaimLockBounds(lockPath, operation, timeout, claimLockSlowThreshold, claimLockEventContext{
		Gaggle: gaggle,
		RunID:  runID,
	}, fn)
}

func claimsLockTimeoutForPath(lockPath string) (time.Duration, error) {
	root := filepath.Dir(filepath.Dir(lockPath))
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return instance.DefaultClaimsLockTimeout, nil
		}
		return 0, err
	}
	return cfg.RunConditions.ClaimsLockTimeoutDuration()
}

func withClaimLockBounds(lockPath, operation string, timeout, slowThreshold time.Duration, eventContext claimLockEventContext, fn func() error) error {
	if operation == "" {
		return errors.New("claim lock operation is required")
	}
	if timeout <= 0 {
		return fmt.Errorf("claim lock timeout must be positive, got %s", timeout)
	}

	waitStarted := time.Now()
	held, err := acquireClaimLock(lockPath, operation, timeout, waitStarted)
	if err != nil {
		var timeoutErr *claimsLockTimeoutError
		if !errors.As(err, &timeoutErr) {
			return err
		}
		journalErr := recordClaimLockTimeout(lockPath, eventContext, timeoutErr)
		outcomeErr := writeClaimLockTimeoutOutcome(timeoutErr)
		if journalErr != nil || outcomeErr != nil {
			return errors.Join(timeoutErr, journalErr, outcomeErr)
		}
		return &journaledClaimsLockTimeoutError{timeout: timeoutErr}
	}

	acquiredAt := time.Now()
	defer func() {
		_ = held.Release()
		releasedAt := time.Now()
		waitDuration := acquiredAt.Sub(waitStarted)
		holdDuration := releasedAt.Sub(acquiredAt)
		if waitDuration > slowThreshold || holdDuration > slowThreshold {
			// The callback may already have durably mutated the ledger. Match
			// claim-transition journaling's best-effort policy rather than
			// reporting a failed operation after that mutation succeeded.
			_ = recordSlowClaimLock(lockPath, operation, waitDuration, holdDuration)
		}
	}()
	return fn()
}

func acquireClaimLock(lockPath, operation string, timeout time.Duration, started time.Time) (*lock.Handle, error) {
	retryInterval := claimLockRetryInterval
	if timeout < retryInterval {
		retryInterval = timeout / 10
		if retryInterval <= 0 {
			retryInterval = time.Nanosecond
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	retry := time.NewTicker(retryInterval)
	defer retry.Stop()

	for {
		held, err := lock.TryAcquire(lockPath)
		switch {
		case err == nil:
			return held, nil
		case !errors.Is(err, lock.ErrHeld):
			return nil, fmt.Errorf("acquire claims lock: %w", err)
		}
		if waited := time.Since(started); waited >= timeout {
			return nil, &claimsLockTimeoutError{
				Operation:    operation,
				Timeout:      timeout,
				WaitDuration: waited,
			}
		}
		select {
		case <-timer.C:
			return nil, &claimsLockTimeoutError{
				Operation:    operation,
				Timeout:      timeout,
				WaitDuration: time.Since(started),
			}
		case <-retry.C:
		}
	}
}

func recordSlowClaimLock(lockPath, operation string, waitDuration, holdDuration time.Duration) error {
	log, _, err := journal.OpenInstanceLog(filepath.Dir(lockPath))
	if err != nil {
		return fmt.Errorf("open instance log for slow claim lock: %w", err)
	}
	if err := log.Append(journal.Event{
		Type: journal.EventClaimLockSlow,
		Runner: map[string]any{
			"operation":    operation,
			"pid":          os.Getpid(),
			"waitDuration": waitDuration.String(),
			"holdDuration": holdDuration.String(),
		},
	}); err != nil {
		_ = log.Close()
		return fmt.Errorf("append slow claim lock event: %w", err)
	}
	if err := log.Close(); err != nil {
		return fmt.Errorf("close instance log after slow claim lock event: %w", err)
	}
	return nil
}

func recordClaimLockTimeout(lockPath string, eventContext claimLockEventContext, timeoutErr *claimsLockTimeoutError) error {
	log, _, err := journal.OpenInstanceLog(filepath.Dir(lockPath))
	if err != nil {
		return fmt.Errorf("open instance log for claim lock timeout: %w", err)
	}
	if err := log.Append(journal.Event{
		Type:     journal.EventClaimLockTimeout,
		Gaggle:   eventContext.Gaggle,
		Workflow: eventContext.Workflow,
		RunID:    eventContext.RunID,
		Error: &journal.ErrorDetail{
			Code:    claimsLockTimeoutCode,
			Message: timeoutErr.Error(),
		},
		Runner: map[string]any{
			"operation":    timeoutErr.Operation,
			"pid":          os.Getpid(),
			"waitDuration": timeoutErr.WaitDuration.String(),
			"timeout":      timeoutErr.Timeout.String(),
			"retryable":    true,
			"failureClass": "infra",
		},
	}); err != nil {
		_ = log.Close()
		return fmt.Errorf("append claim lock timeout event: %w", err)
	}
	if err := log.Close(); err != nil {
		return fmt.Errorf("close instance log after claim lock timeout event: %w", err)
	}
	return nil
}

func writeClaimLockTimeoutOutcome(timeoutErr *claimsLockTimeoutError) error {
	resultFile := providerInput(executor.InputResultFile, "")
	builtinErrorFile := os.Getenv(executor.BuiltinErrorFileEnvVar)
	if resultFile == "" && builtinErrorFile == "" {
		return nil
	}
	data, err := json.Marshal(map[string]any{
		executor.OutputErrorCode:      claimsLockTimeoutCode,
		executor.OutputErrorMessage:   timeoutErr.Error(),
		executor.OutputErrorRetryable: true,
	})
	if err != nil {
		return fmt.Errorf("marshal claim lock timeout result: %w", err)
	}
	var errs []error
	if resultFile != "" {
		if err := os.WriteFile(resultFile, data, 0o644); err != nil {
			errs = append(errs, fmt.Errorf("write claim lock timeout result %s: %w", resultFile, err))
		}
	}
	if builtinErrorFile != "" {
		if err := os.WriteFile(builtinErrorFile, data, 0o600); err != nil {
			errs = append(errs, fmt.Errorf("write claim lock timeout built-in error %s: %w", builtinErrorFile, err))
		}
	}
	return errors.Join(errs...)
}

// withFileLock is the blocking lock used by non-claim cache and merge locks.
// Claim-ledger and blocked-record access must go through withClaimLock instead
// so its wait/hold contention is measured (#791).
//
// BOUNDED, where it used to block forever. It called lock.Acquire directly —
// no timeout, no context — so a stale holder wedged the caller permanently with
// no diagnostic and nothing to time out. Six call sites shared that behaviour
// (merge.lock, the sibling-context / merge-policy / api-read caches, and
// post-merge reconcile), and the bounded discipline already existed 200 lines
// away for claims: acquireClaimLock retries against a deadline and reports the
// operation by name.
//
// Reusing it means a wedged lock now fails with the operation named instead of
// hanging, and the fix is one function rather than six.
func withFileLock(lockPath string, fn func() error) error {
	return withBoundedFileLock(lockPath, "file-lock", fn)
}

// withBoundedFileLock is withFileLock with the operation named for diagnostics.
func withBoundedFileLock(lockPath, operation string, fn func() error) error {
	// The instance's configured claims-lock timeout, falling back to the
	// package default when this lock is not inside an instance root. These are
	// cache and merge locks, so the same bound is the right order of magnitude
	// and stays operator-tunable through one setting rather than two.
	timeout, err := claimsLockTimeoutForPath(lockPath)
	if err != nil || timeout <= 0 {
		timeout = instance.DefaultClaimsLockTimeout
	}
	held, err := acquireClaimLock(lockPath, operation, timeout, time.Now())
	if err != nil {
		return fmt.Errorf("acquire %s lock: %w", operation, err)
	}
	defer func() { _ = held.Release() }()
	return fn()
}
