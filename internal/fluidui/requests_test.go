package fluidui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

type fakeTrigger struct {
	starts   []string
	rejected map[string]string
	err      error
}

func (f *fakeTrigger) Start(_ context.Context, key, _ string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	f.starts = append(f.starts, key)
	return fmt.Sprintf("trigger-%032x", len(f.starts)), "", nil
}

func (f *fakeTrigger) Rejected(_ context.Context, acceptanceID, _ string) (string, bool, error) {
	reason, ok := f.rejected[acceptanceID]
	return reason, ok, nil
}

type fakeRuns map[string]RunState

func (f fakeRuns) read(runID string) (RunState, error) { return f[runID], nil }

func newTestRequests(t *testing.T) (*Requests, fakeRuns, *fakeTrigger) {
	t.Helper()
	runs := fakeRuns{}
	return NewRequests(NewStore(t.TempDir()), runs.read), runs, &fakeTrigger{}
}

func candidateResult(t *testing.T, body string) *CandidateResult {
	t.Helper()
	result := EvaluateCandidate([]byte(body))
	return &result
}

func TestEvaluateCandidateClassifiesOutput(t *testing.T) {
	cases := map[string]struct {
		body, outcome, unmet string
		repassable           bool
	}{
		"candidate":   {`{"version":"portal.customization/v1","density":"compact"}`, OutcomeCandidate, "", false},
		"unsupported": {`{"unsupported":true,"unmetNeed":"Custom Charts!!"}`, OutcomeUnsupported, "custom-charts", false},
		"garbage":     {`I changed the CSS for you`, OutcomeInvalid, "", true},
		"code":        {`{"version":"portal.customization/v1","script":"alert(1)"}`, OutcomeInvalid, "", true},
		"marker+doc":  {`{"unsupported":true,"unmetNeed":"x","density":"compact"}`, OutcomeInvalid, "", true},
		"false flag":  {`{"unsupported":false,"unmetNeed":"x"}`, OutcomeInvalid, "", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := EvaluateCandidate([]byte(tc.body))
			if got.Outcome != tc.outcome || got.UnmetNeed != tc.unmet || got.Repassable != tc.repassable || got.Valid == tc.repassable {
				t.Fatalf("EvaluateCandidate(%s) = %+v", tc.body, got)
			}
			if tc.repassable && got.ErrorSummary == "" {
				t.Fatal("invalid output has no error summary for the retry")
			}
		})
	}
}

func TestNormalizeUnmetNeedBoundsLabels(t *testing.T) {
	cases := map[string]string{
		"":                               UnmetNeedUnlabeled,
		"  !!! ":                         UnmetNeedUnlabeled,
		"Dark Mode":                      "dark-mode",
		"make the whole page bright red": "make-the-whole-page",
		strings.Repeat("a", 100):         strings.Repeat("a", MaxUnmetNeedLen),
	}
	for in, want := range cases {
		if got := NormalizeUnmetNeed(in); got != want {
			t.Errorf("NormalizeUnmetNeed(%q) = %q, want %q", in, got, want)
		}
	}
	if !labelEchoesPrompt("make-the-whole-page", "Make the whole page bright red") {
		t.Fatal("echoed prompt label not detected")
	}
	if labelEchoesPrompt("dark-mode", "dark mode") {
		t.Fatal("short category label treated as an echo")
	}
}

func TestRequestLifecycleFollowsRun(t *testing.T) {
	ctx := context.Background()
	requests, runs, trigger := newTestRequests(t)
	user := UserKey("iss", "alice")
	req, err := requests.Submit(ctx, user, "alice", "  compact please ", trigger)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != StatusQueued || req.Prompt != "compact please" || req.Attempt != 1 || req.RunID == "" {
		t.Fatalf("submitted = %+v", req)
	}
	if len(trigger.starts) != 1 || strings.Contains(trigger.starts[0], "compact") {
		t.Fatalf("trigger keys = %v, want one key without prompt text", trigger.starts)
	}
	input, err := requests.InputForRun(req.RunID)
	if err != nil || input.Request.Text != "compact please" || input.Current != nil || input.Schema != InputSchema {
		t.Fatalf("InputForRun = %+v, %v", input, err)
	}

	runs[req.RunID] = RunState{Phase: journal.PhaseRunning}
	if got, _ := requests.Get(ctx, user, req.ID, trigger); got.Status != StatusImplementing {
		t.Fatalf("running status = %s", got.Status)
	}
	runs[req.RunID] = RunState{Phase: journal.PhaseCompleted, Result: candidateResult(t, `{"version":"portal.customization/v1","density":"compact"}`)}
	got, err := requests.Get(ctx, user, req.ID, trigger)
	if err != nil || got.Status != StatusReadyToPreview || got.Candidate == nil {
		t.Fatalf("completed request = %+v, %v", got, err)
	}
	if _, err := requests.InputForRun(req.RunID); !errors.Is(err, ErrBindingNotFound) {
		t.Fatalf("binding kept after terminal state: %v", err)
	}
	if state, _ := requests.store.Get(user); state.Customization != nil {
		t.Fatal("candidate applied without an explicit apply")
	}

	applied, state, err := requests.Apply(ctx, user, req.ID)
	if err != nil || applied.Status != StatusApplied || state.Customization == nil || state.Customization.Density != DensityCompact {
		t.Fatalf("Apply = %+v, %+v, %v", applied, state, err)
	}
	if _, _, err := requests.Apply(ctx, user, req.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second apply = %v", err)
	}
	reverted, state, err := requests.Revert(ctx, user, req.ID)
	if err != nil || reverted.Status != StatusReverted || state.Customization != nil {
		t.Fatalf("Revert = %+v, %+v, %v", reverted, state, err)
	}
}

func TestRevertRefusesAfterLaterChange(t *testing.T) {
	ctx := context.Background()
	requests, runs, trigger := newTestRequests(t)
	user := UserKey("iss", "alice")
	req, _ := requests.Submit(ctx, user, "alice", "compact", trigger)
	runs[req.RunID] = RunState{Phase: journal.PhaseCompleted, Result: candidateResult(t, `{"version":"portal.customization/v1","density":"compact"}`)}
	if _, _, err := requests.Apply(ctx, user, req.ID); err != nil {
		t.Fatal(err)
	}
	mustPut(t, requests.store, user, validDoc())
	if _, _, err := requests.Revert(ctx, user, req.ID); !errors.Is(err, ErrRevertStale) {
		t.Fatalf("stale revert = %v", err)
	}
}

func TestRequestOutcomes(t *testing.T) {
	ctx := context.Background()
	user := UserKey("iss", "alice")
	cases := map[string]struct {
		prompt string
		state  RunState
		status Status
		unmet  string
	}{
		"unsupported": {"add a cost chart", RunState{Phase: journal.PhaseCompleted, Result: candidateResult(t, `{"unsupported":true,"unmetNeed":"Cost Charts"}`)}, StatusUnsupported, "cost-charts"},
		"echo label":  {"please email me every morning", RunState{Phase: journal.PhaseCompleted, Result: candidateResult(t, `{"unsupported":true,"unmetNeed":"please email me every morning"}`)}, StatusUnsupported, UnmetNeedUnlabeled},
		"aborted":     {"compact", RunState{Phase: journal.PhaseAborted, Result: candidateResult(t, `not json`)}, StatusFailed, ""},
		"no result":   {"compact", RunState{Phase: journal.PhaseCompleted}, StatusFailed, ""},
		"forged":      {"compact", RunState{Phase: journal.PhaseCompleted, Result: &CandidateResult{Outcome: OutcomeCandidate, Candidate: &Customization{Version: "v0"}}}, StatusFailed, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			requests, runs, trigger := newTestRequests(t)
			req, err := requests.Submit(ctx, user, "alice", tc.prompt, trigger)
			if err != nil {
				t.Fatal(err)
			}
			runs[req.RunID] = tc.state
			got, err := requests.Get(ctx, user, req.ID, trigger)
			if err != nil || got.Status != tc.status || got.UnmetNeed != tc.unmet || got.Candidate != nil {
				t.Fatalf("request = %+v, %v", got, err)
			}
			if tc.status == StatusFailed && got.Detail == "" {
				t.Fatal("failure has no detail")
			}
		})
	}
}

func TestRetryStartsFreshAttempt(t *testing.T) {
	ctx := context.Background()
	requests, runs, trigger := newTestRequests(t)
	user := UserKey("iss", "alice")
	req, _ := requests.Submit(ctx, user, "alice", "compact", trigger)
	if _, err := requests.Retry(ctx, user, req.ID, trigger); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("retry while queued = %v", err)
	}
	runs[req.RunID] = RunState{Phase: journal.PhaseFailed}
	retried, err := requests.Retry(ctx, user, req.ID, trigger)
	if err != nil || retried.Status != StatusQueued || retried.Attempt != 2 || retried.RunID == req.RunID || retried.Detail != "" {
		t.Fatalf("Retry = %+v, %v", retried, err)
	}
	if trigger.starts[1] == trigger.starts[0] {
		t.Fatal("retry reused the first attempt's idempotency key")
	}
}

func TestRejectedTriggerFailsRequest(t *testing.T) {
	ctx := context.Background()
	requests, _, trigger := newTestRequests(t)
	user := UserKey("iss", "alice")
	req, _ := requests.Submit(ctx, user, "alice", "compact", trigger)
	if got, _ := requests.Get(ctx, user, req.ID, trigger); got.Status != StatusQueued {
		t.Fatalf("pending acceptance status = %s", got.Status)
	}
	trigger.rejected = map[string]string{"trigger-" + req.RunID: "workflow paused"}
	got, _ := requests.Get(ctx, user, req.ID, trigger)
	if got.Status != StatusFailed || !strings.Contains(got.Detail, "workflow paused") {
		t.Fatalf("rejected request = %+v", got)
	}

	trigger.err = errors.New("no scheduler")
	failed, err := requests.Submit(ctx, user, "alice", "compact", trigger)
	if err != nil || failed.Status != StatusFailed {
		t.Fatalf("refused submit = %+v, %v", failed, err)
	}
	if _, err := requests.Submit(ctx, user, "alice", "compact", nil); !errors.Is(err, ErrTriggerUnavailable) {
		t.Fatalf("submit without trigger = %v", err)
	}
}

func TestRequestsArePrivateAndDurable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	runs := fakeRuns{}
	trigger := &fakeTrigger{}
	alice, bob := UserKey("iss", "alice"), UserKey("iss", "bob")
	req, err := NewRequests(NewStore(dir), runs.read).Submit(ctx, alice, "alice", "secret wish", trigger)
	if err != nil {
		t.Fatal(err)
	}

	restarted := NewRequests(NewStore(dir), runs.read)
	if _, err := restarted.Get(ctx, bob, req.ID, trigger); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("bob Get = %v", err)
	}
	if _, err := restarted.Retry(ctx, bob, req.ID, trigger); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("bob Retry = %v", err)
	}
	if list, _ := restarted.List(ctx, bob, trigger); len(list) != 0 {
		t.Fatalf("bob list = %+v", list)
	}
	list, err := restarted.List(ctx, alice, trigger)
	if err != nil || len(list) != 1 || list[0].ID != req.ID {
		t.Fatalf("alice list after restart = %+v, %v", list, err)
	}
	if _, err := restarted.Get(ctx, alice, "../x", trigger); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("malformed id = %v", err)
	}

	binding, err := os.ReadFile(filepath.Join(dir, "runs", req.RunID+".json"))
	if err != nil || strings.Contains(string(binding), "secret") || strings.Contains(string(binding), "alice") {
		t.Fatalf("binding = %s, %v", binding, err)
	}
	var decoded RunBinding
	if json.Unmarshal(binding, &decoded) != nil || decoded.User != userHash(alice) {
		t.Fatalf("binding = %s", binding)
	}
}

func TestSubmitBoundsRequests(t *testing.T) {
	ctx := context.Background()
	requests, runs, trigger := newTestRequests(t)
	user := UserKey("iss", "alice")
	for _, prompt := range []string{"", strings.Repeat("x", MaxPromptBytes+1), "bell\a"} {
		var verr *ValidationError
		if _, err := requests.Submit(ctx, user, "alice", prompt, trigger); !errors.As(err, &verr) {
			t.Fatalf("Submit(%q) = %v, want validation error", prompt, err)
		}
	}
	var first Request
	for i := range MaxActivePerUser {
		req, err := requests.Submit(ctx, user, "alice", "compact", trigger)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = req
		}
	}
	if _, err := requests.Submit(ctx, user, "alice", "compact", trigger); !errors.Is(err, ErrTooManyRequests) {
		t.Fatalf("over active limit = %v", err)
	}
	runs[first.RunID] = RunState{Phase: journal.PhaseFailed}
	if _, err := requests.Submit(ctx, user, "alice", "compact", trigger); err != nil {
		t.Fatalf("submit after a run finished = %v", err)
	}
}
