package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

type fakePoller struct {
	results []providers.CheckState
	checks  []providers.CheckDetail
	calls   int
}

func (f *fakePoller) PollPullRequest(ctx context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	state := f.results[f.calls]
	if f.calls < len(f.results)-1 {
		f.calls++
	}
	return providers.PullRequestPollResult{CheckState: state, Checks: f.checks}, nil
}

func noSleep(context.Context, time.Duration) error { return nil }

// pollStep is one scripted PollPullRequest outcome for sequencedPoller.
type pollStep struct {
	state providers.CheckState
	err   error
}

// sequencedPoller replays one pollStep per call, staying on the last step
// once exhausted (like fakePoller) — but able to script an error on a given
// call, for #239's transient-error-handling tests.
type sequencedPoller struct {
	steps []pollStep
	calls int
}

func (f *sequencedPoller) PollPullRequest(ctx context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	step := f.steps[f.calls]
	if f.calls < len(f.steps)-1 {
		f.calls++
	}
	return providers.PullRequestPollResult{CheckState: step.state}, step.err
}

func cfgFor(owner, repo, pullID string) CIPollConfig {
	return CIPollConfig{Owner: owner, Repo: repo, PullID: pullID}
}

// TestBackoff_DoublesPerAttemptUpToCap is #122's missing direct unit test
// for the capped-exponential backoff calculation: attempt 0 is base, each
// subsequent attempt doubles, and the cap is enforced once doubling would
// exceed it.
func TestBackoff_DoublesPerAttemptUpToCap(t *testing.T) {
	const base = 10 * time.Second
	const max = 100 * time.Second
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 10 * time.Second},
		{1, 20 * time.Second},
		{2, 40 * time.Second},
		{3, 80 * time.Second},
		{4, 100 * time.Second}, // 160s would exceed max — capped
		{5, 100 * time.Second},
	}
	for _, tc := range cases {
		if got := backoff(base, max, tc.attempt); got != tc.want {
			t.Errorf("backoff(%s, %s, %d) = %s, want %s", base, max, tc.attempt, got, tc.want)
		}
	}
}

// TestBackoff_OverflowSafeAtLargeAttempt proves a large attempt count (the
// left-shift base<<attempt overflowing time.Duration's int64 into a negative
// value) still returns max, not garbage — the `d <= 0` branch in backoff's
// switch exists precisely for this.
func TestBackoff_OverflowSafeAtLargeAttempt(t *testing.T) {
	const base = time.Second
	const max = time.Minute
	if got := backoff(base, max, 100); got != max {
		t.Fatalf("backoff at a large attempt count = %s, want max %s (overflow must not produce a negative/garbage duration)", got, max)
	}
}

func TestCIPollExecutor_Pass(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePassing}}
	recorder := newFakeRecorder()
	exec, err := NewCIPollExecutor(poller, recorder)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (the poll itself succeeded)", result.Status)
	}
	if result.Outputs[OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, result.Outputs[OutputCIStatus], providers.CheckStatePassing)
	}
	if poller.calls != 0 {
		t.Fatalf("expected exactly one poll call, got %d", poller.calls+1)
	}
	if len(result.Artifacts) != 0 || len(recorder.recorded) != 0 {
		t.Fatalf("passing result recorded CI detail artifact: %+v", result.Artifacts)
	}
	if _, ok := result.Outputs[OutputCIFailedChecks]; ok {
		t.Fatalf("passing result has %s output", OutputCIFailedChecks)
	}
}

func TestCIPollExecutor_Fail(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStateFailing}}
	recorder := newFakeRecorder()
	exec, err := NewCIPollExecutor(poller, recorder)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The poll itself succeeded (it determined a terminal state) even though
	// the state it determined is "CI failed" — that verdict rides in Outputs.
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if result.Outputs[OutputCIStatus] != string(providers.CheckStateFailing) {
		t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, result.Outputs[OutputCIStatus], providers.CheckStateFailing)
	}
	if len(result.Artifacts) != 0 || len(recorder.recorded) != 0 {
		t.Fatalf("failure without check details recorded an artifact: %+v", result.Artifacts)
	}
	if _, ok := result.Outputs[OutputCIFailedChecks]; ok {
		t.Fatalf("failure without check details has %s output", OutputCIFailedChecks)
	}
}

func TestCIPollExecutor_FailureEvidenceRoundTrip(t *testing.T) {
	checks := []providers.CheckDetail{
		{Name: "already-passed", State: providers.CheckStatePassing, URL: "https://ci.example/pass", Summary: "ok"},
		{Name: "unit", State: providers.CheckStateFailing, URL: "https://ci.example/unit", Summary: "panic in TestWidget\nfull stack"},
		{Name: "integration", State: providers.CheckStatePending, URL: "https://ci.example/integration", Summary: "still running"},
	}
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStateFailing}, checks: checks}
	recorder := newFakeRecorder()
	exec, err := NewCIPollExecutor(poller, recorder)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs[OutputCIStatus] != string(providers.CheckStateFailing) {
		t.Fatalf("outputs[%s] = %v, want failing", OutputCIStatus, result.Outputs[OutputCIStatus])
	}
	if result.Outputs[OutputCIFailedChecks] != "unit" {
		t.Fatalf("outputs[%s] = %v, want unit", OutputCIFailedChecks, result.Outputs[OutputCIFailedChecks])
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].MediaType != "application/json" {
		t.Fatalf("artifacts = %+v, want one JSON artifact", result.Artifacts)
	}
	data := recorder.recorded[CIChecksArtifactName]
	if result.Artifacts[0].Digest != journal.Digest(data) {
		t.Fatalf("artifact digest = %q, want digest of recorded bytes", result.Artifacts[0].Digest)
	}
	var artifact CIChecksArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode %s: %v", CIChecksArtifactName, err)
	}
	if len(artifact.Checks) != 2 {
		t.Fatalf("checks = %+v, want failing and pending checks only", artifact.Checks)
	}
	if artifact.Checks[0] != checks[1] || artifact.Checks[1] != checks[2] {
		t.Fatalf("checks = %+v, want provider order and complete detail", artifact.Checks)
	}
	if artifact.Metadata.Truncated {
		t.Fatalf("metadata = %+v, want no truncation", artifact.Metadata)
	}
}

func TestCIPollExecutor_BoundsSummariesCheckCountAndScalarOutput(t *testing.T) {
	checks := make([]providers.CheckDetail, 23)
	for i := range checks {
		checks[i] = providers.CheckDetail{
			Name:    fmt.Sprintf("check-%02d-with-a-deliberately-long-name", i),
			State:   providers.CheckStateFailing,
			URL:     fmt.Sprintf("https://ci.example/check/%d", i),
			Summary: strings.Repeat(fmt.Sprintf("%d", i%10), maxCICheckSummaryBytes+100),
		}
	}
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStateFailing}, checks: checks}
	recorder := newFakeRecorder()
	exec, err := NewCIPollExecutor(poller, recorder)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	failedNames, ok := result.Outputs[OutputCIFailedChecks].(string)
	if !ok {
		t.Fatalf("outputs[%s] = %#v, want scalar string", OutputCIFailedChecks, result.Outputs[OutputCIFailedChecks])
	}
	if utf8.RuneCountInString(failedNames) > maxCIFailedChecksOutputRunes ||
		!strings.Contains(failedNames, "…(+") ||
		!strings.HasSuffix(failedNames, " more)") {
		t.Fatalf("bounded failed-check names = %q", failedNames)
	}

	data := recorder.recorded[CIChecksArtifactName]
	var artifact CIChecksArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode %s: %v", CIChecksArtifactName, err)
	}
	if len(artifact.Checks) != maxCIChecks {
		t.Fatalf("retained checks = %d, want %d", len(artifact.Checks), maxCIChecks)
	}
	if artifact.Checks[0].Name != checks[0].Name || artifact.Checks[19].Name != checks[19].Name {
		t.Fatalf("retained checks are not the first %d provider checks", maxCIChecks)
	}
	for i, check := range artifact.Checks {
		if len(check.Summary) != maxCICheckSummaryBytes {
			t.Fatalf("check %d summary bytes = %d, want %d", i, len(check.Summary), maxCICheckSummaryBytes)
		}
	}
	if !artifact.Metadata.Truncated ||
		artifact.Metadata.SummariesTruncated != maxCIChecks ||
		artifact.Metadata.ChecksDropped != len(checks)-maxCIChecks {
		t.Fatalf("metadata = %+v", artifact.Metadata)
	}
}

func TestMarshalCIChecksArtifactPrioritizesFailuresBeforePendingChecks(t *testing.T) {
	checks := make([]providers.CheckDetail, maxCIChecks+1)
	for i := range maxCIChecks {
		checks[i] = providers.CheckDetail{
			Name:  fmt.Sprintf("pending-%02d", i),
			State: providers.CheckStatePending,
		}
	}
	failing := providers.CheckDetail{
		Name:    "failed-after-pending",
		State:   providers.CheckStateFailing,
		URL:     "https://ci.example/failed",
		Summary: "failure details",
	}
	checks[maxCIChecks] = failing

	data, err := marshalCIChecksArtifact(checks)
	if err != nil {
		t.Fatalf("marshalCIChecksArtifact: %v", err)
	}
	var artifact CIChecksArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode %s: %v", CIChecksArtifactName, err)
	}
	if len(artifact.Checks) != maxCIChecks {
		t.Fatalf("retained checks = %d, want %d", len(artifact.Checks), maxCIChecks)
	}
	if artifact.Checks[0] != failing {
		t.Fatalf("first retained check = %+v, want failing check %+v", artifact.Checks[0], failing)
	}
	if artifact.Checks[1] != checks[0] || artifact.Checks[maxCIChecks-1] != checks[maxCIChecks-2] {
		t.Fatalf("pending checks did not retain provider order: %+v", artifact.Checks)
	}
	if !artifact.Metadata.Truncated || artifact.Metadata.ChecksDropped != 1 {
		t.Fatalf("metadata = %+v, want one dropped check", artifact.Metadata)
	}
}

func TestBoundFailedCheckNamesReportsOmittedCount(t *testing.T) {
	first := strings.Repeat("a", 100)
	second := strings.Repeat("b", 100)
	third := strings.Repeat("c", 100)
	want := first + "," + second + ",…(+1 more)"
	if got := boundFailedCheckNames([]string{first, second, third}); got != want {
		t.Fatalf("boundFailedCheckNames = %q, want %q", got, want)
	}
}

func TestBoundFailedCheckNamesCountsPartiallyRetainedNameAsOmitted(t *testing.T) {
	wantMarker := "…(+1 more)"
	got := boundFailedCheckNames([]string{strings.Repeat("a", maxCIFailedChecksOutputRunes+1)})
	if utf8.RuneCountInString(got) != maxCIFailedChecksOutputRunes {
		t.Fatalf("boundFailedCheckNames rune count = %d, want %d", utf8.RuneCountInString(got), maxCIFailedChecksOutputRunes)
	}
	if !strings.HasSuffix(got, wantMarker) {
		t.Fatalf("boundFailedCheckNames = %q, want suffix %q", got, wantMarker)
	}
}

func TestCIPollExecutor_CapsArtifactByDroppingSummariesBeforeChecks(t *testing.T) {
	checks := make([]providers.CheckDetail, maxCIChecks)
	for i := range checks {
		checks[i] = providers.CheckDetail{
			Name:    fmt.Sprintf("check-%02d-%s", i, strings.Repeat("n", 3500)),
			State:   providers.CheckStateFailing,
			URL:     "https://ci.example/" + strings.Repeat("u", 3500),
			Summary: strings.Repeat("s", maxCICheckSummaryBytes+100),
		}
	}
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStateFailing}, checks: checks}
	recorder := newFakeRecorder()
	exec, err := NewCIPollExecutor(poller, recorder)
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep

	if _, err := exec.Run(context.Background(), cfgFor("o", "r", "42")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data := recorder.recorded[CIChecksArtifactName]
	if len(data) > maxCIChecksArtifactBytes {
		t.Fatalf("artifact bytes = %d, limit = %d", len(data), maxCIChecksArtifactBytes)
	}
	var artifact CIChecksArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode %s: %v", CIChecksArtifactName, err)
	}
	if artifact.Metadata.SummariesDropped != maxCIChecks || artifact.Metadata.ChecksDropped == 0 {
		t.Fatalf("metadata = %+v, want all summaries dropped before checks", artifact.Metadata)
	}
	for i, check := range artifact.Checks {
		if check.Summary != "" {
			t.Fatalf("retained check %d summary was not dropped", i)
		}
	}
}

func TestCIPollExecutor_PendingThenPass(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{
		providers.CheckStatePending, providers.CheckStatePending, providers.CheckStatePassing,
	}}
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep
	exec.Timeout = time.Hour // won't be hit

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs[OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, result.Outputs[OutputCIStatus], providers.CheckStatePassing)
	}
	if poller.calls != 2 {
		t.Fatalf("expected 3 poll calls (2 pending + 1 terminal), got %d", poller.calls+1)
	}
}

func TestCIPollExecutor_TimesOutIsAFailure(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePending}}
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep
	exec.Timeout = time.Minute

	base := time.Now()
	tick := 0
	exec.Now = func() time.Time {
		tm := base.Add(time.Duration(tick) * time.Minute)
		tick++
		return tm
	}

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure (the poll itself did not complete)", result.Status)
	}
	if result.Error == nil || result.Error.Code != "poll_timeout" || !result.Error.Retryable {
		t.Fatalf("error = %+v, want poll_timeout, retryable", result.Error)
	}
	// #239: a timeout gets its own distinct ciStatus ("timeout") — neither a
	// claimed pass nor fail — so a downstream ci-status gate check can route
	// it to escalation instead of the "fail" branch's implement repass.
	if result.Outputs[OutputCIStatus] != CIStatusTimeout {
		t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, result.Outputs[OutputCIStatus], CIStatusTimeout)
	}
}

func TestCIPollExecutor_ContextDeadlineReturnsTypedTimeout(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePending}}
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}

	cfg := cfgFor("o", "r", "42")
	cfg.Interval = time.Hour
	cfg.Timeout = 10 * time.Millisecond
	result, err := exec.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "poll_timeout" {
		t.Fatalf("result = %+v, want typed poll_timeout failure", result)
	}
}

// TestCIPollExecutor_TransientErrorsThenPass is the regression test for
// #239 Part 1: a handful of transient provider errors (a 503, a network
// blip) must not abort the poll — the loop backs off and keeps polling,
// eventually reaching the real terminal state.
func TestCIPollExecutor_TransientErrorsThenPass(t *testing.T) {
	poller := &sequencedPoller{steps: []pollStep{
		{err: errors.New("GET .../status failed: status 503: temporarily unavailable")},
		{err: &net.DNSError{Err: "temporary failure", IsTemporary: true}},
		{state: providers.CheckStatePassing},
	}}
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep
	exec.Timeout = time.Hour

	result, err := exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if result.Outputs[OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("outputs[%s] = %v, want %q", OutputCIStatus, result.Outputs[OutputCIStatus], providers.CheckStatePassing)
	}
	if poller.calls != 2 {
		t.Fatalf("expected 3 poll calls (2 transient errors + 1 terminal), got %d", poller.calls+1)
	}
}

// TestCIPollExecutor_NonTransientErrorAbortsImmediately is the negative
// control for #239 Part 1: an error that doesn't look transient (a 404, a
// permissions error) must still abort the poll on the first occurrence —
// only transient-shaped errors get absorbed.
func TestCIPollExecutor_NonTransientErrorAbortsImmediately(t *testing.T) {
	poller := &sequencedPoller{steps: []pollStep{
		{err: errors.New("GET .../status failed: status 404: not found")},
		{state: providers.CheckStatePassing},
	}}
	exec, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep

	_, err = exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err == nil {
		t.Fatal("expected Run to return an error for a non-transient poll failure")
	}
	if poller.calls != 1 {
		t.Fatalf("expected exactly one poll call before aborting, got %d", poller.calls)
	}
}

// TestCIPollExecutor_ConsecutiveTransientErrorsBoundedAbort is the
// bounded-loop regression test for #239: a poller that fails transiently
// forever must not spin until the overall Timeout — it gives up once
// MaxConsecutivePollErrors back-to-back transient errors are seen.
func TestCIPollExecutor_ConsecutiveTransientErrorsBoundedAbort(t *testing.T) {
	alwaysTransient := &alwaysErrorPoller{err: errors.New("GET .../status failed: status 503: down")}
	exec, err := NewCIPollExecutor(alwaysTransient, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	exec.Sleep = noSleep
	exec.Timeout = time.Hour // large enough that only the error budget can end this
	exec.MaxConsecutivePollErrors = 3

	_, err = exec.Run(context.Background(), cfgFor("o", "r", "42"))
	if err == nil {
		t.Fatal("expected Run to give up after exhausting the consecutive-error budget")
	}
	if alwaysTransient.calls != 4 { // 3 absorbed + 1 that trips the budget
		t.Fatalf("expected 4 poll calls (budget=3), got %d", alwaysTransient.calls)
	}
}

// alwaysErrorPoller returns err on every call, for testing the
// consecutive-error budget without needing a Timeout short enough to race.
type alwaysErrorPoller struct {
	err   error
	calls int
}

func (f *alwaysErrorPoller) PollPullRequest(ctx context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	f.calls++
	return providers.PullRequestPollResult{}, f.err
}

func TestCIPollConfigFromEnvelope_MissingLocatorIsError(t *testing.T) {
	if _, err := CIPollConfigFromEnvelope(apiv1.InvocationEnvelope{}); err == nil {
		t.Fatal("expected an error when owner/repo/pullId cannot be determined")
	}
}

func TestCIPollConfigFromEnvelope_DefaultsFromRepoRef(t *testing.T) {
	env := apiv1.InvocationEnvelope{
		RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Inputs:  map[string]interface{}{InputPRNumber: "7"},
	}
	cfg, err := CIPollConfigFromEnvelope(env)
	if err != nil {
		t.Fatalf("CIPollConfigFromEnvelope: %v", err)
	}
	if cfg.Owner != "acme" || cfg.Repo != "widgets" || cfg.PullID != "7" {
		t.Fatalf("cfg = %+v, unexpected", cfg)
	}
}

func TestNewCIPollExecutor_RequiresPoller(t *testing.T) {
	if _, err := NewCIPollExecutor(nil, newFakeRecorder()); err == nil {
		t.Fatal("expected error for nil poller")
	}
}

func TestNewCIPollExecutor_RequiresJournal(t *testing.T) {
	if _, err := NewCIPollExecutor(&fakePoller{}, nil); err == nil {
		t.Fatal("expected error for nil journal")
	}
}

// TestCIPollConfigFromEnvelope_PollIntervalsParseAsDuration proves the #132
// fix: pollIntervalSeconds/pollMaxIntervalSeconds/pollTimeoutSeconds are
// time.ParseDuration strings (e.g. "5m"), not a bare count of seconds — the
// previous implementation appended "s" unconditionally, silently turning
// "5m" into 5 milliseconds.
func TestCIPollConfigFromEnvelope_PollIntervalsParseAsDuration(t *testing.T) {
	env := apiv1.InvocationEnvelope{
		RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Inputs: map[string]interface{}{
			InputPRNumber:           "7",
			InputPollIntervalSec:    "5m",
			InputPollMaxIntervalSec: "1h",
			InputPollTimeoutSec:     "45m",
		},
	}

	cfg, err := CIPollConfigFromEnvelope(env)
	if err != nil {
		t.Fatalf("CIPollConfigFromEnvelope: %v", err)
	}
	if cfg.Interval != 5*time.Minute {
		t.Fatalf("Interval = %s, want 5m", cfg.Interval)
	}
	if cfg.MaxInterval != time.Hour {
		t.Fatalf("MaxInterval = %s, want 1h", cfg.MaxInterval)
	}
	if cfg.Timeout != 45*time.Minute {
		t.Fatalf("Timeout = %s, want 45m", cfg.Timeout)
	}
}

func TestCIPollConfigFromEnvelope_DeclaredLimitCapsLegacyTimeoutInput(t *testing.T) {
	env := apiv1.InvocationEnvelope{
		RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Limits:  apiv1.Limits{MaxDurationSeconds: 12},
		Inputs: map[string]interface{}{
			InputPRNumber:       "7",
			InputPollTimeoutSec: "45m",
		},
	}
	cfg, err := CIPollConfigFromEnvelope(env)
	if err != nil {
		t.Fatalf("CIPollConfigFromEnvelope: %v", err)
	}
	if cfg.Timeout != 11*time.Second {
		t.Fatalf("Timeout = %s, want 11s poll budget within declared 12s stage limit", cfg.Timeout)
	}
}

func TestCIPollConfigFromEnvelope_DeclaredLimitDoesNotLengthenLegacyTimeout(t *testing.T) {
	env := apiv1.InvocationEnvelope{
		RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Limits:  apiv1.Limits{MaxDurationSeconds: 60},
		Inputs: map[string]interface{}{
			InputPRNumber:       "7",
			InputPollTimeoutSec: "5s",
		},
	}
	cfg, err := CIPollConfigFromEnvelope(env)
	if err != nil {
		t.Fatalf("CIPollConfigFromEnvelope: %v", err)
	}
	if cfg.Timeout != 5*time.Second {
		t.Fatalf("Timeout = %s, want shorter legacy poll timeout 5s", cfg.Timeout)
	}
}

// TestCIPollConfigFromEnvelope_MalformedDurationFailsClosed proves a
// malformed poll-cadence input is a hard error, not a silent zero/garbage
// default (the #132 bug: appending "s" then swallowing ParseDuration's error
// let a typo like "5mm" or a bare "5" corrupt silently instead of failing).
func TestCIPollConfigFromEnvelope_MalformedDurationFailsClosed(t *testing.T) {
	env := apiv1.InvocationEnvelope{
		RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Inputs: map[string]interface{}{
			InputPRNumber:        "7",
			InputPollIntervalSec: "not-a-duration",
		},
	}
	if _, err := CIPollConfigFromEnvelope(env); err == nil {
		t.Fatal("expected an error for a malformed pollIntervalSeconds value")
	}
}
