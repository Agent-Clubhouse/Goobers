package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/localscheduler"
)

// A harness group whose sign-in probe failed because the provider could not
// answer (#6017) is not re-probed until its backoff elapses: the provider's
// Retry-After when the probe reported one, else harnessProbeBackoffInitial
// doubling per consecutive transient failure up to harnessProbeBackoffMax.
const (
	harnessProbeBackoffInitial = time.Minute
	harnessProbeBackoffMax     = 30 * time.Minute
)

// harnessProbeThrottle is the process-wide backoff every harness preflight
// consults. It is shared because the worker rebuilds a degraded gaggle kit on
// every activity (#5949), and before #6017 each rebuild re-ran the sign-in
// probe against a provider that had just rate limited it.
var harnessProbeThrottle = newHarnessProbeBackoff(time.Now)

// harnessProbeBackoff records, per harness group, the transient failure that
// is being waited out and when the group may be probed again.
type harnessProbeBackoff struct {
	now func() time.Time

	mu      sync.Mutex
	waiting map[string]harnessProbeWait
}

type harnessProbeWait struct {
	cause   error
	retryAt time.Time
	delay   time.Duration
}

func newHarnessProbeBackoff(now func() time.Time) *harnessProbeBackoff {
	return &harnessProbeBackoff{now: now, waiting: make(map[string]harnessProbeWait)}
}

// pending returns the transient failure key is waiting out, if its backoff
// has not elapsed yet.
func (b *harnessProbeBackoff) pending(key string) (harnessProbeWait, bool) {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	wait, ok := b.waiting[key]
	if !ok || !now.Before(wait.retryAt) {
		return harnessProbeWait{}, false
	}
	return wait, true
}

// due reports whether key may be probed at now.
func (b *harnessProbeBackoff) due(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	wait, ok := b.waiting[key]
	return !ok || !now.Before(wait.retryAt)
}

// record schedules key's next probe after a transient failure and returns
// when that is.
func (b *harnessProbeBackoff) record(key string, cause error) time.Time {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	delay := harnessProbeBackoffInitial
	if previous, ok := b.waiting[key]; ok {
		delay = min(2*previous.delay, harnessProbeBackoffMax)
	}
	wait := delay
	if retryAfter, ok := harness.TransientPreflightRetryAfter(cause); ok {
		wait = retryAfter
	}
	retryAt := now.Add(wait)
	b.waiting[key] = harnessProbeWait{cause: cause, retryAt: retryAt, delay: delay}
	return retryAt
}

// clear forgets key's backoff: its last probe was not a transient failure.
func (b *harnessProbeBackoff) clear(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.waiting, key)
}

// transientHarnessReason is the refusal text for a transient group failure.
func transientHarnessReason(cause error, retryAt time.Time) string {
	return fmt.Sprintf("%v (transient provider failure; next probe after %s)", cause, retryAt.UTC().Format(time.RFC3339))
}

// harnessGroupRecheck re-probes one harness group whose startup preflight
// failed transiently, so the daemon can admit its workflows again once the
// provider recovers instead of refusing them until the next restart or
// configuration change (#6017). A re-probe runs at most once per backoff
// window and never on the scheduler's goroutine. A re-probe that fails for
// any other reason makes the refusal permanent for this configuration, as
// it would have been at startup.
type harnessGroupRecheck struct {
	preflight harnessGroupPreflight
	// start runs a re-probe; production starts a goroutine.
	start func(func())

	mu        sync.Mutex
	reason    string
	permanent bool
	probing   bool
}

func newHarnessGroupRecheck(preflight harnessGroupPreflight, reason string) *harnessGroupRecheck {
	return &harnessGroupRecheck{preflight: preflight, reason: reason, start: func(probe func()) { go probe() }}
}

// current is the group's refusal at now, starting a background re-probe when
// one is due.
func (g *harnessGroupRecheck) current(now time.Time) string {
	g.mu.Lock()
	reason := g.reason
	launch := reason != "" && !g.permanent && !g.probing && g.preflight.throttle.due(g.preflight.key(), now)
	if launch {
		g.probing = true
	}
	g.mu.Unlock()
	if launch {
		g.start(g.reprobe)
	}
	return reason
}

func (g *harnessGroupRecheck) reprobe() {
	outcome := g.preflight.run()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.probing = false
	g.reason = outcome.reason
	g.permanent = outcome.reason != "" && outcome.retryAt.IsZero()
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
func (r *harnessWorkflowRecheck) HarnessRefusal(now time.Time) string {
	refusal := ""
	for _, use := range r.uses {
		if reason := use.group.current(now); reason != "" {
			refusal = appendHarnessRefusal(refusal, fmt.Sprintf("stage %q %s", use.stage, reason))
		}
	}
	return refusal
}
