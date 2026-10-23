package runtimewiring

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/localscheduler"
)

// HarnessProbeThrottle is the process-wide backoff every harness preflight
// consults (#6017). It is shared because the worker rebuilds a degraded gaggle
// kit on every activity (#5949), and before #6017 each rebuild re-ran the
// sign-in probe against a provider that had just rate limited it.
var HarnessProbeThrottle = harness.NewProbeBackoff(time.Now)

// transientHarnessReason is the refusal text for a transient group failure.
func transientHarnessReason(cause error, retryAt time.Time) string {
	return fmt.Sprintf("%v (transient provider failure; next probe after %s)", cause, retryAt.UTC().Format(time.RFC3339))
}

// harnessGroupRecheck re-probes one harness group whose startup preflight
// failed transiently, so the daemon can admit its workflows again once the
// provider recovers instead of refusing them until the next restart or
// configuration change (#6017). A re-probe runs at most once per backoff
// window, never on the scheduler's goroutine, and under the scheduler's
// lifetime context, so daemon shutdown cancels it. Each configuration
// generation builds its own rechecks and the scheduler consults only the
// current generation's, so a re-probe still running for a superseded
// configuration lands in a recheck nothing reads. A re-probe that fails for
// any other reason makes the refusal permanent for this configuration, as
// it would have been at startup.
type harnessGroupRecheck struct {
	Preflight harnessGroupPreflight
	// start runs a re-probe; production starts a goroutine.
	Start func(func())

	Mu        sync.Mutex
	Reason    string
	Key       string
	Permanent bool
	Probing   bool
}

func newHarnessGroupRecheck(preflight harnessGroupPreflight, outcome harnessGroupOutcome) *harnessGroupRecheck {
	return &harnessGroupRecheck{Preflight: preflight, Reason: outcome.Reason, Key: outcome.Key, Start: func(probe func()) { go probe() }}
}

// Current is the group's refusal at now, starting a background re-probe when
// one is due and ctx is still live.
func (g *harnessGroupRecheck) Current(ctx context.Context, now time.Time) string {
	g.Mu.Lock()
	reason := g.Reason
	launch := reason != "" && !g.Permanent && !g.Probing && ctx.Err() == nil && g.Preflight.Throttle.Due(g.Key, now)
	if launch {
		g.Probing = true
	}
	g.Mu.Unlock()
	if launch {
		g.Start(func() { g.Reprobe(ctx) })
	}
	return reason
}

// permanentAfterRecheck tells the operator that a refusal found by a re-probe
// is no longer waited out: it needs the same fix a startup refusal does.
const permanentAfterRecheck = "; a re-probe found a failure that is not transient: fix it (for a sign-in failure, sign in or update the agent:model credential), then reload the configuration or restart the daemon"

// Reprobe rechecks a transient harness refusal when its retry deadline permits.
func (g *harnessGroupRecheck) Reprobe(ctx context.Context) {
	outcome := g.Preflight.Run(ctx)
	g.Mu.Lock()
	defer g.Mu.Unlock()
	g.Probing = false
	if ctx.Err() != nil && outcome.Reason != "" {
		// Shutdown interrupted the probe; its failure says nothing.
		return
	}
	g.Reason, g.Key = outcome.Reason, outcome.Key
	g.Permanent = outcome.Reason != "" && outcome.RetryAt.IsZero()
	if g.Permanent {
		g.Reason += permanentAfterRecheck
	}
}

// HarnessWorkflowRecheck is one workflow's localscheduler.HarnessRecheck: the
// transiently failed harness groups its agentic stages depend on.
type HarnessWorkflowRecheck struct {
	Uses []harnessRecheckUse
}

type harnessRecheckUse struct {
	Stage string
	Group *harnessGroupRecheck
}

// HarnessRefusal implements localscheduler.HarnessRecheck.
func (r *HarnessWorkflowRecheck) HarnessRefusal(ctx context.Context, now time.Time) string {
	refusal := ""
	for _, use := range r.Uses {
		if reason := use.Group.Current(ctx, now); reason != "" {
			refusal = appendHarnessRefusal(refusal, fmt.Sprintf("stage %q %s", use.Stage, reason))
		}
	}
	return refusal
}

var _ localscheduler.HarnessRecheck = (*HarnessWorkflowRecheck)(nil)
