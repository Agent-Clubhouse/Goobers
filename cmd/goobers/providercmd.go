package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// Shared plumbing for the built-in provider-chain stage kinds (#131/#132):
// backlog-query, open-pr, issue-close-out. Unlike every other `goobers`
// subcommand, these are invoked by the runner as a deterministic stage's
// shell command — their process cwd is the stage's worktree, not the
// instance root — so they read their run context from GOOBERS_* env vars
// the runner injects (internal/executor/env.go's buildStageEnv), falling
// back to an explicit [path] argument for standalone/manual invocation.

// newGitHubProvider constructs the GitHub provider these subcommands talk
// to. A package var (not a plain call to providers.NewGitHubProvider) so a
// CLI-level test can point it at an httptest.Server instead of the real
// api.github.com, mirroring runnerwiring.go's newPRPoller/repoCloneURL seams.
var newGitHubProvider = providers.NewGitHubProvider

// claimLedgerFileName/claimLockFileName are the well-known files under an
// instance's scheduler dir the claim ledger and its cross-process lock
// (withClaimLock) live at — shared by backlog-query (the claimant) and
// `goobers up`'s periodic RecoverExpired sweep (the reaper), both of which
// must agree on the same paths.
const (
	claimLedgerFileName = "claims.json"
	claimLockFileName   = "claims.lock"
	// mergeLockFileName is the cross-process flock `merge-pr` (mergepr.go)
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
)

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
	if root := os.Getenv("GOOBERS_INSTANCE_ROOT"); root != "" {
		return root
	}
	return "."
}

// providerRepo loads instance.yaml at root and returns its single configured
// target repo as a providers.RepositoryRef — V0's single-target-repo
// simplification (matching runnerwiring.go's buildCredentials/
// buildCIPollExecutor), and V0 ships GitHub only (instance.Config.Repos[].
// Provider is already validated to "github" at load time).
func providerRepo(root string) (providers.RepositoryRef, error) {
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	if len(cfg.Repos) == 0 {
		return providers.RepositoryRef{}, fmt.Errorf("no repo configured in %s", l.ConfigFile())
	}
	repo := cfg.Repos[0]
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}, nil
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
	runID = os.Getenv("GOOBERS_RUN_ID")
	workflow = os.Getenv("GOOBERS_WORKFLOW")
	if runID == "" {
		return "", "", fmt.Errorf("GOOBERS_RUN_ID is not set — this subcommand must run as a workflow stage")
	}
	if workflow == "" {
		return "", "", fmt.Errorf("GOOBERS_WORKFLOW is not set — this subcommand must run as a workflow stage")
	}
	return runID, workflow, nil
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
	errorCodeAuthFailed = "github_auth_failed"
	// errorCodeNetwork is either a transport-level failure (dial/DNS/reset/
	// timeout) that exhausted send()'s own in-request retry budget, or any
	// other condition providers.IsTransientError recognizes without a
	// status code attached — retryable, since the failure is unrelated to
	// the request's content.
	errorCodeNetwork = "network_error"
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
	var rl *providers.RateLimitError
	if errors.As(err, &rl) {
		extra = map[string]interface{}{}
		if !rl.Reset.IsZero() {
			extra["rateLimitReset"] = rl.Reset.UTC().Format(time.RFC3339)
		}
		if rl.Secondary {
			return errorCodeSecondaryRateLimited, true, extra
		}
		return providers.ErrorCodeRateLimited, true, extra
	}
	if status, ok := statusCodeFrom(err); ok {
		switch {
		case status == http.StatusUnauthorized, status == http.StatusForbidden:
			return errorCodeAuthFailed, false, nil
		case status >= 500:
			return errorCodeServerError, true, nil
		}
	}
	if providers.IsTransientError(err) {
		return errorCodeNetwork, true, nil
	}
	return errorCodeProvider, false, nil
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
	data, merr := json.Marshal(payload)
	if merr != nil {
		pf(stderr, "warning: marshal typed error result: %v\n", merr)
		return 1
	}
	if werr := os.WriteFile(resultFile, data, 0o644); werr != nil {
		pf(stderr, "warning: write typed error result %s: %v\n", resultFile, werr)
	}
	return 1
}

// withClaimLock serializes fn against every other process (a concurrent
// `goobers backlog-query` from a racing run, or `goobers up`'s periodic
// RecoverExpired) touching the same instance's claim ledger, via a blocking
// exclusive flock on lockPath. localscheduler.ClaimLedger's own mutex is
// documented as in-process only ("designed for one embedded scheduler per
// instance... not cross-process file locking") — but backlog-query runs as
// its own OS process per stage dispatch, so two concurrent runs'
// backlog-query subprocesses each open an independent ClaimLedger instance
// against the SAME claims.json file: without this lock, both could read the
// file before either persists, and the second write would silently clobber
// the first's claim (a lost update), defeating "one claim wins" (#131's
// acceptance criterion). A blocking (not acquireInstanceLock's non-blocking)
// flock is deliberate: the loser here should wait its turn and get a
// consistent read, not fail outright the way a second `goobers up` should.
func withClaimLock(lockPath string, fn func() error) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open claim lock file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire claim lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
