package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/boundedwait"
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
	OutputPRNumber       = InputPRNumber
	CIChecksArtifactName = "ci-checks.json"
)

const (
	maxCIFailedChecksOutputRunes = 256
	maxCICheckSummaryBytes       = 1 << 10
	maxCIChecksArtifactBytes     = 64 << 10
	maxCIChecks                  = 20
	// Per-check annotation bounds. A failing matrix job can emit hundreds of
	// annotations; the first few carry the diagnosis and the rest are noise
	// that would crowd out the other failing checks under the artifact budget.
	maxCICheckAnnotations       = 10
	maxCIAnnotationMessageBytes = 1 << 10
)

// CIChecksArtifact is the durable, bounded per-check evidence emitted when CI
// fails. Checks prioritize failures, retain provider order within each
// priority, and exclude passing checks.
type CIChecksArtifact struct {
	Checks   []CICheck                `json:"checks"`
	Metadata CIChecksArtifactMetadata `json:"metadata"`
}

// CICheck is one failing check as recorded in the evidence artifact: the
// provider's own check detail plus its annotations.
//
// Annotations are the file/line/message diagnostics the provider already
// attaches to a failing check run, and for most CI systems they are the ONLY
// machine-readable statement of what went wrong — GitHub Actions leaves
// output.summary empty unless a job writes one, which the common `go test`
// job does not. Without them the evidence handed to a repass is a check name
// and a URL the stage cannot fetch, so the agent has nothing to act on and
// repeats its diff (#1972). The provider already retrieves them
// (providers.CIFailures); only the wire type dropped them.
//
// Embedded rather than added to providers.CheckDetail so the existing JSON
// keys are unchanged and providers.CIFailureDetail — which declares its own
// Annotations — keeps one unambiguous field.
type CICheck struct {
	providers.CheckDetail
	Annotations      []providers.CheckAnnotation `json:"annotations,omitempty"`
	HostReproduction CIHostReproduction          `json:"hostReproduction"`
}

// CIHostReproduction tells a repass whether the current runner can reproduce a
// platform-specific check locally.
type CIHostReproduction struct {
	Status        string `json:"status"`
	HostPlatform  string `json:"hostPlatform"`
	CheckPlatform string `json:"checkPlatform,omitempty"`
	Diagnostic    string `json:"diagnostic"`
}

// CIChecksArtifactMetadata records every lossy bound applied while curating a
// ci-checks.json artifact.
type CIChecksArtifactMetadata struct {
	Truncated                   bool `json:"truncated"`
	SummariesTruncated          int  `json:"summariesTruncated,omitempty"`
	SummariesDropped            int  `json:"summariesDropped,omitempty"`
	AnnotationMessagesTruncated int  `json:"annotationMessagesTruncated,omitempty"`
	AnnotationsDropped          int  `json:"annotationsDropped,omitempty"`
	ChecksDropped               int  `json:"checksDropped,omitempty"`
}

// CIStatusTimeout is the OutputCIStatus value CIPollExecutor sets when it
// gives up waiting for a terminal check state before the overall Timeout
// expires — deliberately distinct from providers.CheckStatePassing/
// CheckStateFailing (#239) so a downstream ci-status gate check can route a
// stalled/slow CI queue separately from the "fail" branch's implement repass.
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
	InputPollTimeoutSec     = boundedwait.InputPollTimeout
	// InputHumanPolicyIDs declares the branch/required-check policy identities
	// the agent loop cannot fix (human/merge-time policies: merge strategy,
	// required reviewers, comment resolution, proof-of-presence). A rejection
	// on one of these does NOT drive the fail branch — only a policy the agent
	// can fix does. Accepts a YAML list or a comma-separated string; the values
	// are provider-interpreted (the ADO provider matches branch-policy
	// configuration ids). Unset means every required policy gates.
	InputHumanPolicyIDs = "humanPolicyConfigurationIds"
)

// Default poll cadence for CIPollExecutor: capped exponential backoff and an
// overall timeout, mirroring the shape (not the exact constants, which are
// GitHub-response-header-specific and unexported) of providers' own
// rate-limit backoff.
const (
	DefaultPollInterval    = 15 * time.Second
	DefaultMaxPollInterval = 2 * time.Minute
	DefaultPollTimeout     = boundedwait.DefaultPollTimeout
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
	HumanPolicyIDs                 []string
}

// CIPollConfigFromEnvelope builds a CIPollConfig from the well-known Input*
// keys in env.Inputs, defaulting owner/repo from env.RepoRef when not
// explicitly given (the PR under poll is almost always in the run's own
// target repo). InputPRNumber is required — how it got into Inputs (e.g. an
// earlier "open PR" task's output, threaded through by the workflow/runner)
// is outside this package's concern.
func CIPollConfigFromEnvelope(env apiv1.InvocationEnvelope) (CIPollConfig, error) {
	cfg := CIPollConfig{
		Owner:          stringInput(env, InputPROwner),
		Repo:           stringInput(env, InputPRRepo),
		PullID:         stringInput(env, InputPRNumber),
		HumanPolicyIDs: stringSliceInput(env, InputHumanPolicyIDs),
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
		pollBudget := boundedwait.CIPollBudget(stageBudget)
		if cfg.Timeout <= 0 || cfg.Timeout > pollBudget {
			cfg.Timeout = pollBudget
		}
	}
	return cfg, nil
}

// stringSliceInput reads key as a list of strings, accepting either a YAML list
// (preserved as []interface{}/[]string on the in-process envelope) or a single
// comma-separated string. Empty/blank entries are dropped.
func stringSliceInput(env apiv1.InvocationEnvelope, key string) []string {
	v, ok := env.Inputs[key]
	if !ok || v == nil {
		return nil
	}
	appendNonEmpty := func(dst []string, s string) []string {
		if s = strings.TrimSpace(s); s != "" {
			dst = append(dst, s)
		}
		return dst
	}
	var out []string
	switch t := v.(type) {
	case []string:
		for _, s := range t {
			out = appendNonEmpty(out, s)
		}
	case []interface{}:
		for _, e := range t {
			out = appendNonEmpty(out, fmt.Sprint(e))
		}
	case string:
		for _, s := range strings.Split(t, ",") {
			out = appendNonEmpty(out, s)
		}
	default:
		out = appendNonEmpty(out, fmt.Sprint(t))
	}
	return out
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
		Repository:                  providers.RepositoryRef{Owner: cfg.Owner, Name: cfg.Repo},
		PullID:                      cfg.PullID,
		HumanPolicyConfigurationIDs: cfg.HumanPolicyIDs,
	}

	consecutiveErrors := 0
	for attempt := 0; ; attempt++ {
		result, err := e.Poller.PollPullRequest(ctx, req)
		invoke.ReportProgress(ctx)
		if err != nil {
			if ciPollDeadlineExceeded(parentCtx, ctx) {
				return ciPollTimeoutOutcome(timeout, cfg.PullID), nil
			}
			if !providers.IsTransientError(err) {
				return apiv1.ResultEnvelope{}, fmt.Errorf("executor: poll pull request: %w", &ciPollProviderError{cause: err})
			}
			consecutiveErrors++
			if consecutiveErrors > maxConsecutiveErrors {
				return apiv1.ResultEnvelope{}, fmt.Errorf("executor: poll pull request: %d consecutive transient errors, giving up: %w", consecutiveErrors, err)
			}
			if now().After(deadline) {
				return ciPollTimeoutOutcome(timeout, cfg.PullID), nil
			}
			if serr := sleep(ctx, backoff(interval, maxInterval, attempt)); serr != nil {
				if ciPollDeadlineExceeded(parentCtx, ctx) {
					return ciPollTimeoutOutcome(timeout, cfg.PullID), nil
				}
				return apiv1.ResultEnvelope{}, serr
			}
			continue
		}
		consecutiveErrors = 0
		switch result.CheckState {
		case providers.CheckStatePassing:
			return ciPollOutcome(providers.CheckStatePassing, "ci-poll: checks passing", cfg.PullID), nil
		case providers.CheckStateFailing:
			return e.ciPollFailureOutcome(ctx, cfg, result)
		}
		if now().After(deadline) {
			return ciPollTimeoutOutcome(timeout, cfg.PullID), nil
		}
		if err := sleep(ctx, backoff(interval, maxInterval, attempt)); err != nil {
			if ciPollDeadlineExceeded(parentCtx, ctx) {
				return ciPollTimeoutOutcome(timeout, cfg.PullID), nil
			}
			return apiv1.ResultEnvelope{}, err
		}
	}
}

func (e *CIPollExecutor) ciPollFailureOutcome(ctx context.Context, cfg CIPollConfig, result providers.PullRequestPollResult) (apiv1.ResultEnvelope, error) {
	outcome := ciPollOutcome(providers.CheckStateFailing, "ci-poll: checks failing", cfg.PullID)
	names := failingCheckNames(result.Checks)
	if len(names) == 0 {
		return outcome, nil
	}

	data, err := marshalCIChecksArtifact(result.Checks, e.failingCheckAnnotations(ctx, cfg, result))
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: encode %s: %w", CIChecksArtifactName, err)
	}
	recorder, ok := e.Journal.(integrityArtifactRecorder)
	if !ok {
		return apiv1.ResultEnvelope{}, errors.New("executor: CI failure evidence integrity recorder is unavailable")
	}
	ref, err := recorder.RecordArtifactWithIntegrity(CIChecksArtifactName, data, apiv1.IntegrityUnapproved)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record %s: %w", CIChecksArtifactName, err)
	}
	outcome.Outputs[OutputCIFailedChecks] = boundFailedCheckNames(names)
	outcome.Artifacts = []apiv1.ArtifactPointer{artifactPointer(ref, "application/json")}
	return outcome, nil
}

// CIFailureLister is the optional provider capability that returns failing
// checks with their annotations. Optional rather than part of PRPoller because
// not every provider exposes annotations, and a provider that does not must
// keep working exactly as before — the artifact simply carries no annotations.
type CIFailureLister interface {
	CIFailures(ctx context.Context, repo providers.RepositoryRef, ref string) ([]providers.CIFailureDetail, error)
}

// failingCheckAnnotations resolves annotations for the failing checks, keyed by
// check name. Called once, on the terminal failing outcome — never on the
// polling path, so a run that never fails costs no extra provider calls.
//
// Best-effort by design: annotations enrich the evidence, they are not the
// outcome. A provider that cannot supply them, or an error fetching them, must
// not turn a determined CI verdict into a stage failure.
func (e *CIPollExecutor) failingCheckAnnotations(ctx context.Context, cfg CIPollConfig, result providers.PullRequestPollResult) map[string][]providers.CheckAnnotation {
	lister, ok := e.Poller.(CIFailureLister)
	if !ok || result.HeadSHA == "" {
		return nil
	}
	failures, err := lister.CIFailures(ctx, providers.RepositoryRef{Owner: cfg.Owner, Name: cfg.Repo}, result.HeadSHA)
	if err != nil {
		return nil
	}
	annotations := make(map[string][]providers.CheckAnnotation, len(failures))
	for _, failure := range failures {
		if len(failure.Annotations) > 0 {
			annotations[failure.Name] = failure.Annotations
		}
	}
	return annotations
}

// boundCheckAnnotations caps how much of one check's annotation set reaches the
// artifact, so a matrix job emitting hundreds cannot crowd out sibling checks.
func boundCheckAnnotations(annotations []providers.CheckAnnotation) ([]providers.CheckAnnotation, int, int) {
	if len(annotations) == 0 {
		return nil, 0, 0
	}
	bounded := make([]providers.CheckAnnotation, 0, min(len(annotations), maxCICheckAnnotations))
	messagesTruncated := 0
	for _, annotation := range annotations {
		if len(bounded) == maxCICheckAnnotations {
			break
		}
		annotation.Path = strings.ToValidUTF8(annotation.Path, "�")
		annotation.Title = strings.ToValidUTF8(annotation.Title, "�")
		annotation.Level = strings.ToValidUTF8(annotation.Level, "�")
		annotation.Message = strings.ToValidUTF8(annotation.Message, "�")
		if truncated, did := truncateUTF8Bytes(annotation.Message, maxCIAnnotationMessageBytes); did {
			annotation.Message = truncated
			messagesTruncated++
		}
		bounded = append(bounded, annotation)
	}
	return bounded, len(annotations) - len(bounded), messagesTruncated
}

func classifyHostReproduction(checkName, hostPlatform string) CIHostReproduction {
	checkPlatform := checkPlatformFromName(checkName)
	reproduction := CIHostReproduction{
		Status:       "unknown",
		HostPlatform: hostPlatform,
		Diagnostic:   fmt.Sprintf("host reproducibility unknown from check name; current host is %s", hostPlatform),
	}
	if checkPlatform == "" {
		return reproduction
	}
	reproduction.CheckPlatform = checkPlatform
	if checkPlatform == hostPlatform {
		reproduction.Status = "reproducible"
		reproduction.Diagnostic = fmt.Sprintf("check targets %s and is reproducible on the current host", checkPlatform)
		return reproduction
	}
	reproduction.Status = "not-reproducible"
	reproduction.Diagnostic = fmt.Sprintf("check targets %s and cannot be reproduced on the current %s host", checkPlatform, hostPlatform)
	return reproduction
}

func checkPlatformFromName(name string) string {
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	platforms := make(map[string]struct{}, 1)
	for _, word := range words {
		switch word {
		case "linux", "ubuntu":
			platforms["linux"] = struct{}{}
		case "darwin", "macos":
			platforms["darwin"] = struct{}{}
		case "windows", "win32":
			platforms["windows"] = struct{}{}
		}
	}
	if len(platforms) != 1 {
		return ""
	}
	for platform := range platforms {
		return platform
	}
	return ""
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

func marshalCIChecksArtifact(checks []providers.CheckDetail, annotations map[string][]providers.CheckAnnotation) ([]byte, error) {
	artifact := CIChecksArtifact{
		Checks: make([]CICheck, 0, min(len(checks), maxCIChecks)),
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
		entry := CICheck{
			CheckDetail:      check,
			HostReproduction: classifyHostReproduction(check.Name, runtime.GOOS),
		}
		var annotationsDropped, messagesTruncated int
		entry.Annotations, annotationsDropped, messagesTruncated = boundCheckAnnotations(annotations[check.Name])
		artifact.Metadata.AnnotationsDropped += annotationsDropped
		artifact.Metadata.AnnotationMessagesTruncated += messagesTruncated
		artifact.Checks = append(artifact.Checks, entry)
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
	artifact.Metadata.Truncated = artifact.Metadata.ChecksDropped > 0 ||
		artifact.Metadata.SummariesTruncated > 0 ||
		artifact.Metadata.AnnotationMessagesTruncated > 0 ||
		artifact.Metadata.AnnotationsDropped > 0

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
	// Annotations outrank summaries and are shed only after every summary is
	// gone: a file/line/message is the most actionable thing in the artifact,
	// and for a job that writes no output.summary it is the only one.
	for i := len(artifact.Checks) - 1; i >= 0 && len(data) > maxCIChecksArtifactBytes; i-- {
		if len(artifact.Checks[i].Annotations) == 0 {
			continue
		}
		artifact.Metadata.AnnotationsDropped += len(artifact.Checks[i].Annotations)
		artifact.Checks[i].Annotations = nil
		data, err = json.Marshal(artifact)
		if err != nil {
			return nil, err
		}
	}
	for len(data) > maxCIChecksArtifactBytes && len(artifact.Checks) > 0 {
		artifact.Metadata.AnnotationsDropped += len(artifact.Checks[len(artifact.Checks)-1].Annotations)
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
		Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: mediaType, Integrity: ref.Integrity,
	}
}

func ciPollDeadlineExceeded(parentCtx, pollCtx context.Context) bool {
	return errors.Is(pollCtx.Err(), context.DeadlineExceeded) && parentCtx.Err() == nil
}

// ciPollTimeoutOutcome builds the ResultEnvelope for a poll that exhausted
// its Timeout while still pending. It preserves the PR number so a workflow
// can checkpoint and re-enter ci-poll without losing the pull request context.
func ciPollTimeoutOutcome(timeout time.Duration, pullID string) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Outputs: map[string]interface{}{OutputCIStatus: CIStatusTimeout, OutputPRNumber: pullID},
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
// vocabulary ("passing"/"failing"), not apiv1.ResultStatus. The PR number is
// passed through for workflow loops that poll the same pull request again.
func ciPollOutcome(checkState providers.CheckState, summary, pullID string) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]interface{}{OutputCIStatus: string(checkState), OutputPRNumber: pullID},
		Summary: summary,
	}
}

// backoff returns a jittered duration between half and all of base<<attempt,
// with the exponential ceiling capped at max.
func backoff(base, max time.Duration, attempt int) time.Duration {
	ceiling := base << attempt
	if ceiling <= 0 || ceiling > max {
		ceiling = max
	}
	floor := ceiling / 2
	return floor + time.Duration(rand.Int64N(int64(ceiling-floor)+1))
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
