package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// OutputCIStatus is the ResultEnvelope.Outputs key CIPollExecutor sets to the
// polled PR's terminal check state, as a string matching providers.CheckState
// ("passing"/"failing") — the contract internal/gate's "ci-status" check
// (#20) reads to branch the repass loop. This is the providers vocabulary
// (the raw check state PollPullRequestResult.CheckState already carries),
// not apiv1.ResultStatus's "success"/"failure" — the two were previously
// conflated (#132), which left ci-status unable to ever match a gate
// declaring params.equals: "passing" (providers' own vocabulary, and what
// both shipped implementation.yaml workflows declare). ci-poll's own
// ResultEnvelope.Status reflects whether it *successfully determined* an
// outcome, not the outcome itself: a failing CI check is still a successful
// poll.
const (
	OutputCIStatus       = "ciStatus"
	OutputCIFailedChecks = "ciFailedChecks"
	CIChecksArtifactName = "ci-checks.json"
)

const (
	maxCIFailedChecksOutputRunes = 256
	maxCICheckSummaryBytes       = 1 << 10
	maxCIChecksArtifactBytes     = 64 << 10
	maxCIChecks                  = 20
)

// CIChecksArtifact is the durable, bounded per-check evidence emitted when CI
// fails. Checks prioritize failures, retain provider order within each
// priority, and exclude passing checks.
type CIChecksArtifact struct {
	Checks   []providers.CheckDetail  `json:"checks"`
	Metadata CIChecksArtifactMetadata `json:"metadata"`
}

// CIChecksArtifactMetadata records every lossy bound applied while curating a
// ci-checks.json artifact.
type CIChecksArtifactMetadata struct {
	Truncated          bool `json:"truncated"`
	SummariesTruncated int  `json:"summariesTruncated,omitempty"`
	SummariesDropped   int  `json:"summariesDropped,omitempty"`
	ChecksDropped      int  `json:"checksDropped,omitempty"`
}

// CIStatusTimeout is the OutputCIStatus value CIPollExecutor sets when it
// gives up waiting for a terminal check state before the overall Timeout
// expires — deliberately distinct from providers.CheckStatePassing/
// CheckStateFailing (#239) so a downstream ci-status gate check can route a
// stalled/slow CI queue to escalation instead of the "fail" branch's
// implement repass, which was the worst possible response to CI merely being
// slow: re-implementing a change that was never actually reviewed as failing.
const CIStatusTimeout = "timeout"

// Well-known Task.Inputs keys a ci-poll stage may declare (see
// ConfigFromEnvelope/CIPollConfigFromEnvelope and doc.go's note on how the PR
// locator gets there).
const (
	InputPROwner  = "prOwner"
	InputPRRepo   = "prRepo"
	InputPRNumber = "prNumber"
	// InputPollIntervalSec/InputPollMaxIntervalSec/InputPollTimeoutSec are
	// time.ParseDuration strings (e.g. "15s", "5m") despite the "Sec"
	// suffix — matching shell.go's InputTimeout convention, not a bare
	// integer count of seconds.
	InputPollIntervalSec    = "pollIntervalSeconds"
	InputPollMaxIntervalSec = "pollMaxIntervalSeconds"
	InputPollTimeoutSec     = "pollTimeoutSeconds"
)

// Default poll cadence for CIPollExecutor: capped exponential backoff and an
// overall timeout, mirroring the shape (not the exact constants, which are
// GitHub-response-header-specific and unexported) of providers' own
// rate-limit backoff.
const (
	DefaultPollInterval    = 15 * time.Second
	DefaultMaxPollInterval = 2 * time.Minute
	DefaultPollTimeout     = 30 * time.Minute
	ciPollResultMargin     = time.Second
)

// DefaultMaxConsecutivePollErrors bounds how many transient poll errors
// (providers.IsTransientError) CIPollExecutor absorbs back-to-back before
// giving up — without this bound, a poller that fails transiently forever
// (e.g. a PR whose CI checks were permanently misconfigured to 503) would
// poll until the overall Timeout regardless, silently burning the full 30
// minutes on every attempt instead of failing fast once it's clear the
// errors aren't clearing.
const DefaultMaxConsecutivePollErrors = 5

// PRPoller is the narrow slice of providers.RepoProvider CIPollExecutor
// depends on, so it can be driven by a fake in tests instead of a real
// GitHub/ADO client.
type PRPoller interface {
	PollPullRequest(ctx context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error)
}

type ciPollProviderError struct {
	cause error
}

func (e *ciPollProviderError) Error() string { return e.cause.Error() }
func (e *ciPollProviderError) Unwrap() error { return e.cause }

// CIPollConfig configures one ci-poll stage invocation.
type CIPollConfig struct {
	Owner, Repo, PullID            string
	Interval, MaxInterval, Timeout time.Duration
}

// CIPollConfigFromEnvelope builds a CIPollConfig from the well-known Input*
// keys in env.Inputs, defaulting owner/repo from env.RepoRef when not
// explicitly given (the PR under poll is almost always in the run's own
// target repo). InputPRNumber is required — how it got into Inputs (e.g. an
// earlier "open PR" task's output, threaded through by the workflow/runner)
// is outside this package's concern.
func CIPollConfigFromEnvelope(env apiv1.InvocationEnvelope) (CIPollConfig, error) {
	cfg := CIPollConfig{
		Owner:  stringInput(env, InputPROwner),
		Repo:   stringInput(env, InputPRRepo),
		PullID: stringInput(env, InputPRNumber),
	}
	if cfg.Owner == "" {
		cfg.Owner = env.RepoRef.Owner
	}
	if cfg.Repo == "" {
		cfg.Repo = env.RepoRef.Name
	}
	if cfg.Owner == "" || cfg.Repo == "" || cfg.PullID == "" {
		return CIPollConfig{}, errors.New("executor: ci-poll requires owner/repo (or env.repoRef) and " + InputPRNumber)
	}
	var err error
	if cfg.Interval, err = durationInput(env, InputPollIntervalSec); err != nil {
		return CIPollConfig{}, err
	}
	if cfg.MaxInterval, err = durationInput(env, InputPollMaxIntervalSec); err != nil {
		return CIPollConfig{}, err
	}
	if cfg.Timeout, err = durationInput(env, InputPollTimeoutSec); err != nil {
		return CIPollConfig{}, err
	}
	if env.Limits.MaxDurationSeconds > 0 {
		stageBudget := time.Duration(env.Limits.MaxDurationSeconds) * time.Second
		pollBudget := ciPollBudget(stageBudget)
		if cfg.Timeout <= 0 || cfg.Timeout > pollBudget {
			cfg.Timeout = pollBudget
		}
	}
	return cfg, nil
}

// ciPollBudget leaves time for a typed timeout result to cross the stage
// boundary before the runner's enclosing wall-clock limit expires.
func ciPollBudget(stage time.Duration) time.Duration {
	margin := ciPollResultMargin
	if margin >= stage {
		margin = stage / 10
	}
	if budget := stage - margin; budget > 0 {
		return budget
	}
	return stage / 2
}

// durationInput parses key's declared value as a time.ParseDuration string
// (e.g. "15s", "5m"), mirroring shell.go's timeoutFor: an unset key returns
// the zero Duration (the caller's own default applies), but a SET, malformed
// value fails closed with a real error rather than silently defaulting to
// zero — the previous behavior here (appending "s" unconditionally, e.g.
// turning a "5m" typo into 5 milliseconds, and swallowing ParseDuration's
// error entirely) let a misconfigured poll cadence corrupt silently (#132).
func durationInput(env apiv1.InvocationEnvelope, key string) (time.Duration, error) {
	s := stringInput(env, key)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("executor: invalid %s input %q: %w", key, s, err)
	}
	return d, nil
}

// CIPollExecutor implements the ci-poll built-in deterministic-stage kind: it
// polls a pull request's combined CI/check state to a terminal outcome with
// capped exponential backoff and reports it via OutputCIStatus for a
// downstream automated gate to branch on.
type CIPollExecutor struct {
	Poller PRPoller
	// Journal records bounded per-check failure evidence. Required.
	Journal ArtifactRecorder
	// Interval/MaxInterval/Timeout are this executor's defaults; a positive
	// value on CIPollConfig overrides them per call.
	Interval    time.Duration
	MaxInterval time.Duration
	Timeout     time.Duration
	// MaxConsecutivePollErrors bounds back-to-back transient poll errors
	// before Run gives up early rather than waiting out the full Timeout.
	// Defaults to DefaultMaxConsecutivePollErrors when <= 0.
	MaxConsecutivePollErrors int
	// Now and Sleep are injectable for deterministic tests; nil defaults to
	// the real wall clock.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// NewCIPollExecutor builds a CIPollExecutor with real-clock defaults.
func NewCIPollExecutor(poller PRPoller, recorder ArtifactRecorder) (*CIPollExecutor, error) {
	if poller == nil {
		return nil, errors.New("executor: poller must not be nil")
	}
	if recorder == nil {
		return nil, errors.New("executor: journal must not be nil")
	}
	return &CIPollExecutor{Poller: poller, Journal: recorder}, nil
}

// Run polls to a terminal check state or until cfg's timeout expires.
//
// A terminal passing/failing check state is a *successful* poll — Status is
// always ResultSuccess and Outputs[OutputCIStatus] carries which terminal
// state was reached ("success"/"failure"), for a downstream gate to branch
// on. Exhausting the timeout while still pending is a genuine stage failure
// (Retryable: true) — the poll itself did not complete, which is a different
// outcome from "CI finished and failed".
func (e *CIPollExecutor) Run(ctx context.Context, cfg CIPollConfig) (apiv1.ResultEnvelope, error) {
	if cfg.Owner == "" || cfg.Repo == "" || cfg.PullID == "" {
		return apiv1.ResultEnvelope{}, errors.New("executor: ci-poll requires owner, repo, and pullId")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = e.Interval
	}
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	maxInterval := cfg.MaxInterval
	if maxInterval <= 0 {
		maxInterval = e.MaxInterval
	}
	if maxInterval <= 0 {
		maxInterval = DefaultMaxPollInterval
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = e.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultPollTimeout
	}
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	maxConsecutiveErrors := e.MaxConsecutivePollErrors
	if maxConsecutiveErrors <= 0 {
		maxConsecutiveErrors = DefaultMaxConsecutivePollErrors
	}

	now := e.Now
	if now == nil {
		now = time.Now
	}
	sleep := e.Sleep
	if sleep == nil {
		sleep = contextSleep
	}

	deadline := now().Add(timeout)
	req := providers.PullRequestPollRequest{
		Repository: providers.RepositoryRef{Owner: cfg.Owner, Name: cfg.Repo},
		PullID:     cfg.PullID,
	}

	consecutiveErrors := 0
	for attempt := 0; ; attempt++ {
		result, err := e.Poller.PollPullRequest(ctx, req)
		invoke.ReportProgress(ctx)
		if err != nil {
			if ciPollDeadlineExceeded(parentCtx, ctx) {
				return ciPollTimeoutOutcome(timeout), nil
			}
			if !providers.IsTransientError(err) {
				return apiv1.ResultEnvelope{}, fmt.Errorf("executor: poll pull request: %w", &ciPollProviderError{cause: err})
			}
			consecutiveErrors++
			if consecutiveErrors > maxConsecutiveErrors {
				return apiv1.ResultEnvelope{}, fmt.Errorf("executor: poll pull request: %d consecutive transient errors, giving up: %w", consecutiveErrors, err)
			}
			if now().After(deadline) {
				return ciPollTimeoutOutcome(timeout), nil
			}
			if serr := sleep(ctx, backoff(interval, maxInterval, attempt)); serr != nil {
				if ciPollDeadlineExceeded(parentCtx, ctx) {
					return ciPollTimeoutOutcome(timeout), nil
				}
				return apiv1.ResultEnvelope{}, serr
			}
			continue
		}
		consecutiveErrors = 0
		switch result.CheckState {
		case providers.CheckStatePassing:
			return ciPollOutcome(providers.CheckStatePassing, "ci-poll: checks passing"), nil
		case providers.CheckStateFailing:
			return e.ciPollFailureOutcome(result)
		}
		if now().After(deadline) {
			return ciPollTimeoutOutcome(timeout), nil
		}
		if err := sleep(ctx, backoff(interval, maxInterval, attempt)); err != nil {
			if ciPollDeadlineExceeded(parentCtx, ctx) {
				return ciPollTimeoutOutcome(timeout), nil
			}
			return apiv1.ResultEnvelope{}, err
		}
	}
}

func (e *CIPollExecutor) ciPollFailureOutcome(result providers.PullRequestPollResult) (apiv1.ResultEnvelope, error) {
	outcome := ciPollOutcome(providers.CheckStateFailing, "ci-poll: checks failing")
	names := failingCheckNames(result.Checks)
	if len(names) == 0 {
		return outcome, nil
	}

	data, err := marshalCIChecksArtifact(result.Checks)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: encode %s: %w", CIChecksArtifactName, err)
	}
	ref, err := e.Journal.RecordArtifact(CIChecksArtifactName, data)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record %s: %w", CIChecksArtifactName, err)
	}
	outcome.Outputs[OutputCIFailedChecks] = boundFailedCheckNames(names)
	outcome.Artifacts = []apiv1.ArtifactPointer{artifactPointer(ref, "application/json")}
	return outcome, nil
}

func failingCheckNames(checks []providers.CheckDetail) []string {
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		if check.State == providers.CheckStateFailing {
			names = append(names, strings.ToValidUTF8(check.Name, "\uFFFD"))
		}
	}
	return names
}

func boundFailedCheckNames(names []string) string {
	joined := strings.Join(names, ",")
	if utf8.RuneCountInString(joined) <= maxCIFailedChecksOutputRunes {
		return joined
	}

	for retained := len(names) - 1; retained > 0; retained-- {
		marker := fmt.Sprintf("…(+%d more)", len(names)-retained)
		candidate := strings.Join(names[:retained], ",") + "," + marker
		if utf8.RuneCountInString(candidate) <= maxCIFailedChecksOutputRunes {
			return candidate
		}
	}

	marker := fmt.Sprintf("…(+%d more)", len(names))
	budget := maxCIFailedChecksOutputRunes - utf8.RuneCountInString(marker)
	if budget <= 0 {
		return marker
	}
	first := []rune(names[0])
	if len(first) > budget {
		first = first[:budget]
	}
	return string(first) + marker
}

func marshalCIChecksArtifact(checks []providers.CheckDetail) ([]byte, error) {
	artifact := CIChecksArtifact{
		Checks: make([]providers.CheckDetail, 0, min(len(checks), maxCIChecks)),
	}
	nonPassing := 0
	for _, check := range checks {
		if check.State != providers.CheckStatePassing {
			nonPassing++
		}
	}
	appendCheck := func(check providers.CheckDetail) {
		if len(artifact.Checks) == maxCIChecks {
			return
		}
		check.Name = strings.ToValidUTF8(check.Name, "\uFFFD")
		check.URL = strings.ToValidUTF8(check.URL, "\uFFFD")
		check.Summary = strings.ToValidUTF8(check.Summary, "\uFFFD")
		if bounded, truncated := truncateUTF8Bytes(check.Summary, maxCICheckSummaryBytes); truncated {
			check.Summary = bounded
			artifact.Metadata.SummariesTruncated++
		}
		artifact.Checks = append(artifact.Checks, check)
	}
	for _, check := range checks {
		if check.State == providers.CheckStateFailing {
			appendCheck(check)
		}
	}
	for _, check := range checks {
		if check.State != providers.CheckStatePassing && check.State != providers.CheckStateFailing {
			appendCheck(check)
		}
	}
	artifact.Metadata.ChecksDropped = nonPassing - len(artifact.Checks)
	artifact.Metadata.Truncated = artifact.Metadata.ChecksDropped > 0 || artifact.Metadata.SummariesTruncated > 0

	data, err := json.Marshal(artifact)
	if err != nil {
		return nil, err
	}
	if len(data) <= maxCIChecksArtifactBytes {
		return data, nil
	}

	artifact.Metadata.Truncated = true
	for i := len(artifact.Checks) - 1; i >= 0 && len(data) > maxCIChecksArtifactBytes; i-- {
		if artifact.Checks[i].Summary == "" {
			continue
		}
		artifact.Checks[i].Summary = ""
		artifact.Metadata.SummariesDropped++
		data, err = json.Marshal(artifact)
		if err != nil {
			return nil, err
		}
	}
	for len(data) > maxCIChecksArtifactBytes && len(artifact.Checks) > 0 {
		artifact.Checks = artifact.Checks[:len(artifact.Checks)-1]
		artifact.Metadata.ChecksDropped++
		data, err = json.Marshal(artifact)
		if err != nil {
			return nil, err
		}
	}
	if len(data) > maxCIChecksArtifactBytes {
		return nil, fmt.Errorf("metadata exceeds %d-byte artifact limit", maxCIChecksArtifactBytes)
	}
	return data, nil
}

func truncateUTF8Bytes(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end], true
}

func artifactPointer(ref journal.Ref, mediaType string) apiv1.ArtifactPointer {
	return apiv1.ArtifactPointer{
		Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: mediaType,
	}
}

func ciPollDeadlineExceeded(parentCtx, pollCtx context.Context) bool {
	return errors.Is(pollCtx.Err(), context.DeadlineExceeded) && parentCtx.Err() == nil
}

// ciPollTimeoutOutcome builds the ResultEnvelope for a poll that exhausted
// its Timeout while still pending. Outputs[OutputCIStatus] is set to
// CIStatusTimeout — distinct from "passing"/"failing" — so a downstream
// ci-status gate check can route it to escalation rather than the "fail"
// branch's implement repass (#239): CI merely being slow is not the same
// evidence as CI having actually failed, and re-implementing in response
// wastes an agentic attempt on the worst possible diagnosis.
func ciPollTimeoutOutcome(timeout time.Duration) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Outputs: map[string]interface{}{OutputCIStatus: CIStatusTimeout},
		Error: &apiv1.ErrorInfo{
			Code:      "poll_timeout",
			Message:   fmt.Sprintf("ci-poll timed out after %s waiting for a terminal check state", timeout),
			Retryable: true,
		},
		Summary: "ci-poll timed out while still pending",
	}
}

// ciPollOutcome builds the ResultEnvelope for a poll that reached a terminal
// state: the stage itself always succeeded (it determined an outcome); the
// outcome is carried in Outputs[OutputCIStatus] using the providers.CheckState
// vocabulary ("passing"/"failing"), not apiv1.ResultStatus.
func ciPollOutcome(checkState providers.CheckState, summary string) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]interface{}{OutputCIStatus: string(checkState)},
		Summary: summary,
	}
}

// backoff returns base<<attempt capped at max, matching the shape of the
// repo's other capped-exponential backoff (providers.backoffDuration).
func backoff(base, max time.Duration, attempt int) time.Duration {
	d := base << attempt
	if d <= 0 || d > max {
		return max
	}
	return d
}

// contextSleep waits for d or until ctx is cancelled, whichever comes first —
// this package's own copy of the pattern providers.contextSleep uses
// (unexported there, so not importable).
func contextSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
