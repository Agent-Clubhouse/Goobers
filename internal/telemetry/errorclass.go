package telemetry

import "strings"

// ErrorClass is a normalized, queryable error category (TEL-012/#22) so the
// work-nomination workflow can query failure patterns across runs without
// parsing free-text messages. It is derived from the journal error event's
// stable machine-readable Code (internal/journal.ErrorDetail.Code) via
// ClassifyError — this package never imports internal/journal (decoupled, same
// playbook as #12's provider seams); it only documents the code convention a
// runner/provider-adapter should follow.
type ErrorClass string

// The error taxonomy. Kept small and stable: broad enough for nomination to
// query meaningfully, narrow enough that every runner-emitted code has an
// unambiguous home.
const (
	ErrorClassHarnessFailure    ErrorClass = "harness-failure"
	ErrorClassProviderRateLimit ErrorClass = "provider-rate-limit"
	ErrorClassTimeout           ErrorClass = "timeout"
	ErrorClassValidation        ErrorClass = "validation"
	ErrorClassInfra             ErrorClass = "infra"
	ErrorClassUnknown           ErrorClass = "unknown"

	// The classes below split what used to be the single "unknown" bucket by
	// WHO OWNS the failure, which is the question an operator actually asks
	// of a failure count. Before them a gaggle whose token could not clone,
	// whose credential helper would not exec, and whose DNS was down reported
	// one indistinguishable number (2026-08-08 reliability audit).

	// ErrorClassProvider is a provider refusing or failing a request for a
	// reason that is not a rate limit — auth, permission, a malformed call.
	ErrorClassProvider ErrorClass = "provider"
	// ErrorClassInfraGit is a git-reported clone/fetch/worktree failure that
	// is not a transport failure.
	ErrorClassInfraGit ErrorClass = "infra_git"
	// ErrorClassInfraNet is a failure to reach the remote at all.
	ErrorClassInfraNet ErrorClass = "infra_net"
	// ErrorClassInfraLock is contention on the instance's claims lock.
	ErrorClassInfraLock ErrorClass = "infra_lock"
	// ErrorClassExecutor is a genuine runner/executor defect — the residual
	// left once every recognized external cause has its own class.
	ErrorClassExecutor ErrorClass = "executor"
	// ErrorClassItemJudgment is a stage's correct terminal conclusion about
	// the ITEM it was handed, not a failure of the work (#3363): the item is
	// stale, already done, or otherwise not applicable. Re-running the stage
	// can only re-derive the same conclusion, and counting it as a failure
	// scores the machine down for being right (#3364).
	ErrorClassItemJudgment ErrorClass = "item-judgment"
)

// Well-known error codes. These are the exact internal/journal.ErrorDetail.Code
// values a runner or provider adapter should emit so ClassifyError resolves
// them without falling back to heuristics. ErrCodeProviderRateLimit in
// particular is the code the runner should use when adapting a
// providers.RateLimitObserver event (#12) into a journal error event / span
// event — keeping the two mission's telemetry consistent.
const (
	ErrCodeProviderRateLimit = "provider.rate_limit"
	ErrCodeTimeout           = "timeout"
	ErrCodeHarnessFailure    = "harness.failure"
	ErrCodeValidationFailed  = "validation.failed"
	ErrCodeInfraFailure      = "infra.failure"
)

// Runner- and executor-authored codes. These are emitted today by
// internal/runner (workspace provisioning), internal/executor (provider
// stages), and the provider-chain subcommands (claims lock, GitHub adapters).
// They are listed here rather than left to the heuristics below because each
// one has an exact, non-negotiable home — and because every one of them
// classified as "unknown" until this map named it.
const (
	ErrCodeInfraGit       = "infra_git_failed"
	ErrCodeInfraNet       = "infra_net_failed"
	ErrCodeInfraWorkspace = "infra_workspace_failed"
	// ErrCodeInfraJournal identifies an upstream stage's artifact that this
	// run's journal could not produce (#4121). It is the SUBSTRATE failing to
	// carry a value between two stages — on the pod arm the producer emits an
	// artifact op over the journal plane and a different pod reads it back —
	// and nothing a work item can contain makes that read fail, so it must
	// never accumulate failure-streak strikes against the item.
	ErrCodeInfraJournal   = "infra_journal_failed"
	ErrCodeClaimsLock     = "claims_lock_timeout"
	ErrCodeExecutor       = "executor_error"
	ErrCodeProviderFailed = "provider_error"
	ErrCodePollProvider   = "poll_provider_error"
	ErrCodeGitHubAuth     = "github_auth_failed"
	// ErrCodeCredentialUnavailable identifies a declared credential whose
	// configured source cannot currently be materialized.
	ErrCodeCredentialUnavailable = "credential_unavailable"
	// ErrCodeNoWorkUnsubstantiated is the runner's refusal of a no-work claim
	// no upstream stage delivered evidence for (#2736). Spelled here so the
	// refusal is reportable in operator health numbers instead of landing in
	// the unknown bucket; internal/runner.NoWorkUnsubstantiatedCode is this
	// same constant.
	ErrCodeNoWorkUnsubstantiated = "NO_WORK_UNSUBSTANTIATED"
)

// Agent-authored item-judgment codes (#3363). Spelled here so the runner's
// routing policy, the journal surface, and the rollup's disposition split all
// share one vocabulary (#3364) instead of three hand-synced literals.
const (
	// ErrCodeIssueNotApplicable is an implementer's verified refusal: the
	// issue's premise no longer holds (targets deleted files, work already
	// done, would reintroduce removed code). A correct refusal is a terminal
	// deliverable about the item, never a work failure.
	ErrCodeIssueNotApplicable = "ISSUE_NOT_APPLICABLE"
)

var wellKnownErrorCodes = map[string]ErrorClass{
	ErrCodeProviderRateLimit: ErrorClassProviderRateLimit,
	ErrCodeTimeout:           ErrorClassTimeout,
	ErrCodeHarnessFailure:    ErrorClassHarnessFailure,
	ErrCodeValidationFailed:  ErrorClassValidation,
	ErrCodeInfraFailure:      ErrorClassInfra,
	ErrCodeInfraGit:          ErrorClassInfraGit,
	ErrCodeInfraNet:          ErrorClassInfraNet,
	ErrCodeInfraWorkspace:    ErrorClassInfra,
	ErrCodeInfraJournal:      ErrorClassInfra,
	// Exact, so it beats the "timeout" substring heuristic below: waiting out
	// another process's claims lock is contention, not a stage running long,
	// and the two want different remedies.
	ErrCodeClaimsLock:            ErrorClassInfraLock,
	ErrCodeExecutor:              ErrorClassExecutor,
	ErrCodeProviderFailed:        ErrorClassProvider,
	ErrCodePollProvider:          ErrorClassProvider,
	ErrCodeGitHubAuth:            ErrorClassProvider,
	ErrCodeCredentialUnavailable: ErrorClassInfra,
	ErrCodeIssueNotApplicable:    ErrorClassItemJudgment,
	// The run's evidence contract, not the work: the claim did not hold up
	// against what the journal says its upstream produced.
	ErrCodeNoWorkUnsubstantiated: ErrorClassValidation,
}

// InfraFault reports whether c names an infrastructure fault — a failure of
// the substrate the work runs on (credentials, git, network, host, lock
// contention), which carries no evidence about the WORK or the ITEM (#3361).
// Deliberately excludes ErrorClassTimeout: a session running past its
// wall-clock budget is the failure-streak notifier's motivating case
// (#1054), not weather. Consumers use this to keep infra weather out of
// work-quality signals: the failure-streak circuit breaker and the
// success-rate denominator (#3364).
func (c ErrorClass) InfraFault() bool {
	switch c {
	case ErrorClassInfra, ErrorClassInfraGit, ErrorClassInfraNet, ErrorClassInfraLock:
		return true
	}
	return false
}

// ClassifyError normalizes a journal error event's Code into an ErrorClass.
// Empty code classifies as empty (no error). An exact well-known code always
// wins; otherwise a small set of prefix/substring heuristics covers codes that
// follow the documented dotted-namespace convention without matching exactly.
// Anything else is ErrorClassUnknown rather than guessed at, so nomination
// queries can distinguish "known-unclassifiable" from "not yet seen."
func ClassifyError(code string) ErrorClass {
	if code == "" {
		return ""
	}
	if class, ok := wellKnownErrorCodes[code]; ok {
		return class
	}
	lower := strings.ToLower(code)
	switch {
	case strings.Contains(lower, "rate_limit"), strings.Contains(lower, "rate-limit"):
		return ErrorClassProviderRateLimit
	case strings.HasPrefix(lower, "infra_git"):
		return ErrorClassInfraGit
	case strings.HasPrefix(lower, "infra_net"):
		return ErrorClassInfraNet
	// Ordered ahead of the "timeout" substring so a lock wait keeps its own
	// class even under a code this map does not list exactly.
	case strings.Contains(lower, "lock_timeout"):
		return ErrorClassInfraLock
	case strings.Contains(lower, "timeout"):
		return ErrorClassTimeout
	case strings.HasPrefix(lower, "harness."):
		return ErrorClassHarnessFailure
	case strings.HasPrefix(lower, "validation."), strings.HasPrefix(lower, "schema."):
		return ErrorClassValidation
	case strings.HasPrefix(lower, "infra."), strings.HasPrefix(lower, "infra_"):
		return ErrorClassInfra
	// Provider adapters namespace their codes by provider (github_*), so a
	// code this map has not yet caught up with still classifies to its owner
	// instead of falling into "unknown".
	case strings.HasPrefix(lower, "github_"), strings.HasPrefix(lower, "provider_"):
		return ErrorClassProvider
	default:
		return ErrorClassUnknown
	}
}
