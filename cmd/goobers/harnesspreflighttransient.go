package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/localscheduler"
)

// harnessProbeThrottle is the process-wide backoff every harness preflight
// consults (#6017). It is shared because the worker rebuilds a degraded gaggle
// kit on every activity (#5949), and before #6017 each rebuild re-ran the
// sign-in probe against a provider that had just rate limited it.
var harnessProbeThrottle = harness.NewProbeBackoff(time.Now)

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
	preflight harnessGroupPreflight
	// start runs a re-probe; production starts a goroutine.
	start func(func())

	mu        sync.Mutex
	reason    string
	key       string
	permanent bool
	probing   bool
}

func newHarnessGroupRecheck(preflight harnessGroupPreflight, outcome harnessGroupOutcome) *harnessGroupRecheck {
	return &harnessGroupRecheck{preflight: preflight, reason: outcome.reason, key: outcome.key, start: func(probe func()) { go probe() }}
}

// current is the group's refusal at now, starting a background re-probe when
// one is due and ctx is still live.
func (g *harnessGroupRecheck) current(ctx context.Context, now time.Time) string {
	g.mu.Lock()
	reason := g.reason
	launch := reason != "" && !g.permanent && !g.probing && ctx.Err() == nil && g.preflight.throttle.Due(g.key, now)
	if launch {
		g.probing = true
	}
	g.mu.Unlock()
	if launch {
		g.start(func() { g.reprobe(ctx) })
	}
	return reason
}

// permanentAfterRecheck tells the operator that a refusal found by a re-probe
// is no longer waited out: it needs the same fix a startup refusal does.
const permanentAfterRecheck = "; a re-probe found a failure that is not transient: fix it (for a sign-in failure, sign in or update the agent:model credential), then reload the configuration or restart the daemon"

func (g *harnessGroupRecheck) reprobe(ctx context.Context) {
	outcome := g.preflight.run(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.probing = false
	if ctx.Err() != nil && outcome.reason != "" {
		// Shutdown interrupted the probe; its failure says nothing.
		return
	}
	g.reason, g.key = outcome.reason, outcome.key
	g.permanent = outcome.reason != "" && outcome.retryAt.IsZero()
	if g.permanent {
		g.reason += permanentAfterRecheck
	}
}

// harnessWorkflowRecheck is one workflow's localscheduler.HarnessRecheck: the
// transiently failed harness groups its agentic stages depend on.
type harnessWorkflowRecheck struct {
	uses []harnessRecheckUse
}

type harnessRecheckUse struct {
	stage string
	group *harnessGroupRecheck
}

var _ localscheduler.HarnessRecheck = (*harnessWorkflowRecheck)(nil)

// HarnessRefusal implements localscheduler.HarnessRecheck.
func (r *harnessWorkflowRecheck) HarnessRefusal(ctx context.Context, now time.Time) string {
	refusal := ""
	for _, use := range r.uses {
		if reason := use.group.current(ctx, now); reason != "" {
			refusal = appendHarnessRefusal(refusal, fmt.Sprintf("stage %q %s", use.stage, reason))
		}
	}
	return refusal
}
