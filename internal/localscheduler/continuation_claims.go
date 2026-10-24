package localscheduler

import (
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ContinuationClaimTTL bounds how long a parked parent's refused resume holds
// capacity against new admissions without retrying (#8174). Each refused
// retry renews it, so a parent that keeps asking keeps its place; one whose
// owner died stops holding capacity after at most this long, leaving one slot
// idle for one TTL at worst. Fixed for now; it is not an operator setting.
const ContinuationClaimTTL = 10 * time.Minute

// continuationClaim is a parked parent waiting to resume: its resume was
// refused for capacity, so the next slot that frees is its (#8174).
type continuationClaim struct {
	identity WorkflowIdentity
	// maxConcurrent is the claimer's workflow cap when it last asked: a claim
	// holds an instance slot only while its own workflow has room for it.
	maxConcurrent int
	since         time.Time
	until         time.Time
}

// olderThan reports whether claim a (run aRun) was made before claim b (run
// bRun); run IDs break ties so the order is total.
func olderThan(a continuationClaim, aRun string, b continuationClaim, bRun string) bool {
	return a.since.Before(b.since) || a.since.Equal(b.since) && aRun < bRun
}

func (c *Conditions) clockNow() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// pruneClaimsLocked drops expired claims. c.mu is held.
func (c *Conditions) pruneClaimsLocked(now time.Time) {
	for runID, claim := range c.claims {
		if !now.Before(claim.until) {
			delete(c.claims, runID)
		}
	}
}

// heldByClaimsLocked counts the live claims that hold capacity against an
// admission: every claim for a new start, and for the resume of a claiming
// run only the claims older than its own, so waiting parents resume first
// come, first served. A claim always holds its own workflow's slot, but it
// holds an instance slot only while its workflow has room for it (active
// runs plus older claims of the same workflow below its cap): a parent still
// waiting on its own workflow cannot use an instance slot, so it must not
// freeze one against other workflows. c.mu is held.
func (c *Conditions) heldByClaimsLocked(identity WorkflowIdentity, runID string) (workflow, instance int) {
	own, claiming := c.claims[runID]
	for other, claim := range c.claims {
		if other == runID || claiming && !olderThan(claim, other, own, runID) {
			continue
		}
		if claim.identity == identity {
			workflow++
		}
		if c.claimCanUseInstanceLocked(other, claim) {
			instance++
		}
	}
	return workflow, instance
}

// claimCanUseInstanceLocked reports whether the claim's own workflow has a
// slot for it, so an instance slot would let it resume. c.mu is held.
func (c *Conditions) claimCanUseInstanceLocked(runID string, claim continuationClaim) bool {
	ahead := 0
	for other, older := range c.claims {
		if other != runID && older.identity == claim.identity && olderThan(older, other, claim, runID) {
			ahead++
		}
	}
	return c.active[claim.identity]+ahead < claim.maxConcurrent
}

// markContinuationWaitingLocked records or renews runID's claim. c.mu is held.
func (c *Conditions) markContinuationWaitingLocked(runID string, identity WorkflowIdentity, maxConcurrent int, now time.Time) {
	if c.claims == nil {
		c.claims = map[string]continuationClaim{}
	}
	claim, ok := c.claims[runID]
	if !ok || claim.identity != identity {
		claim = continuationClaim{identity: identity, since: now}
	}
	// A renewal keeps the claim's place: since is never reset.
	claim.maxConcurrent, claim.until = maxConcurrent, now.Add(ContinuationClaimTTL)
	c.claims[runID] = claim
}

// ClearContinuationClaim drops runID's claim: it resumed, or its admission
// ended (cancelled, terminalized, superseded) and can never resume.
func (c *Conditions) ClearContinuationClaim(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.claims, runID)
}

// reserveParentContinuation reacquires concurrency for the parked parent
// runID, ahead of new admissions and of parents that started waiting later.
// A capacity refusal records or renews the parent's claim; success clears it.
func (c *Conditions) reserveParentContinuation(runID string, identity WorkflowIdentity, r apiv1.ReadinessConditions) (ok bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clockNow()
	c.pruneClaimsLocked(now)
	ok, reason = c.reserveContinuationLocked(identity, r, runID)
	if ok {
		delete(c.claims, runID)
		return ok, reason
	}
	if reason == ReasonMaxParallel || reason == ReasonInstanceMaxParallel {
		c.markContinuationWaitingLocked(runID, identity, maxConcurrentRuns(r), now)
	}
	return ok, reason
}

// maxConcurrentRuns is a workflow's concurrency cap, defaulting to one.
func maxConcurrentRuns(r apiv1.ReadinessConditions) int {
	if r.MaxConcurrentRuns <= 0 {
		return 1
	}
	return int(r.MaxConcurrentRuns)
}
