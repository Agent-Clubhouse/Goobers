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
	since    time.Time
	until    time.Time
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
// admission: every claim for a new start (except runID's own), and, for the
// resume of a claiming run, only the claims older than its own, so waiting
// parents resume first come first served. c.mu is held.
func (c *Conditions) heldByClaimsLocked(identity WorkflowIdentity, runID string) (workflow, instance int) {
	own, claiming := c.claims[runID]
	for other, claim := range c.claims {
		if other == runID {
			continue
		}
		if claiming && !claim.since.Before(own.since) && (claim.since.After(own.since) || other > runID) {
			continue
		}
		instance++
		if claim.identity == identity {
			workflow++
		}
	}
	return workflow, instance
}

// markContinuationWaitingLocked records or renews runID's claim. c.mu is held.
func (c *Conditions) markContinuationWaitingLocked(runID string, identity WorkflowIdentity, now time.Time) {
	if c.claims == nil {
		c.claims = map[string]continuationClaim{}
	}
	claim, ok := c.claims[runID]
	if !ok || claim.identity != identity {
		claim = continuationClaim{identity: identity, since: now}
	}
	claim.until = now.Add(ContinuationClaimTTL)
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
		c.markContinuationWaitingLocked(runID, identity, now)
	}
	return ok, reason
}
