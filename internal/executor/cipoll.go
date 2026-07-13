package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// CICheckName is the AutomatedGate.Check value CIPollEvaluator handles: the
// "ci-poll" built-in primitive that drives the implementation workflow's
// CI-poll/repass loop (GT-020).
const CICheckName = "ci-poll"

// Default poll cadence for CIPollEvaluator: capped exponential backoff and an
// overall timeout, mirroring the shape (not the exact constants, which are
// GitHub-response-header-specific and unexported) of providers' own
// rate-limit backoff.
const (
	DefaultPollInterval    = 15 * time.Second
	DefaultMaxPollInterval = 2 * time.Minute
	DefaultPollTimeout     = 30 * time.Minute
)

// PRPoller is the narrow slice of providers.RepoProvider CIPollEvaluator
// depends on, so it can be driven by a fake in tests instead of a real
// GitHub/ADO client.
type PRPoller interface {
	PollPullRequest(ctx context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error)
}

// CIPollEvaluator implements invoke.Automated for AutomatedGate{Check:
// "ci-poll"}: it polls a pull request's combined CI/check state to a
// terminal outcome with capped exponential backoff and maps it to the gate's
// "pass"/"fail" branch outcome.
type CIPollEvaluator struct {
	Poller PRPoller
	// Interval/MaxInterval/Timeout override the package defaults when
	// positive; the gate's own Params override both.
	Interval    time.Duration
	MaxInterval time.Duration
	Timeout     time.Duration
	// Now and Sleep are injectable for deterministic tests; nil defaults to
	// the real wall clock.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// NewCIPollEvaluator builds a CIPollEvaluator with real-clock defaults.
func NewCIPollEvaluator(poller PRPoller) (*CIPollEvaluator, error) {
	if poller == nil {
		return nil, errors.New("executor: poller must not be nil")
	}
	return &CIPollEvaluator{Poller: poller}, nil
}

// Evaluate implements invoke.Automated. gate.Check must be CICheckName.
// gate.Params.pullId is required; owner/repo default to env.RepoRef when not
// given in Params. Optional Params "intervalSeconds"/"maxIntervalSeconds"/
// "timeoutSeconds" override this evaluator's cadence for this one gate.
//
// Evaluate blocks, polling to a terminal state or until its timeout expires.
// A terminal passing/failing check state returns "pass"/"fail" with a nil
// error (GT-020's declared branch outcome). Running out of time while still
// pending is NOT "fail" — it's inconclusive, so it comes back as an error
// instead of conflating "CI hasn't finished" with "CI failed".
func (e *CIPollEvaluator) Evaluate(ctx context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	if gate.Check != CICheckName {
		return "", fmt.Errorf("executor: CIPollEvaluator does not handle check %q", gate.Check)
	}
	owner, repo, pullID, err := prLocator(gate.Params, env)
	if err != nil {
		return "", err
	}
	interval := durationParam(gate.Params, "intervalSeconds", e.Interval, DefaultPollInterval)
	maxInterval := durationParam(gate.Params, "maxIntervalSeconds", e.MaxInterval, DefaultMaxPollInterval)
	timeout := durationParam(gate.Params, "timeoutSeconds", e.Timeout, DefaultPollTimeout)

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
		Repository: providers.RepositoryRef{Owner: owner, Name: repo},
		PullID:     pullID,
	}

	for attempt := 0; ; attempt++ {
		result, err := e.Poller.PollPullRequest(ctx, req)
		if err != nil {
			return "", fmt.Errorf("executor: poll pull request: %w", err)
		}
		switch result.CheckState {
		case providers.CheckStatePassing:
			return "pass", nil
		case providers.CheckStateFailing:
			return "fail", nil
		}
		if now().After(deadline) {
			return "", fmt.Errorf("executor: ci-poll timed out after %s waiting for a terminal check state", timeout)
		}
		if err := sleep(ctx, backoff(interval, maxInterval, attempt)); err != nil {
			return "", err
		}
	}
}

func prLocator(params map[string]string, env apiv1.InvocationEnvelope) (owner, repo, pullID string, err error) {
	owner = params["owner"]
	if owner == "" {
		owner = env.RepoRef.Owner
	}
	repo = params["repo"]
	if repo == "" {
		repo = env.RepoRef.Name
	}
	pullID = params["pullId"]
	if owner == "" || repo == "" || pullID == "" {
		return "", "", "", errors.New("executor: ci-poll gate requires owner/repo (or env.repoRef) and params.pullId")
	}
	return owner, repo, pullID, nil
}

// durationParam reads key from params as whole seconds, falling back to
// override (if positive) and then def. A malformed or non-positive value in
// params is treated as absent rather than an error: these are advisory
// cadence knobs, not correctness-critical.
func durationParam(params map[string]string, key string, override, def time.Duration) time.Duration {
	if s := params[key]; s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if override > 0 {
		return override
	}
	return def
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
