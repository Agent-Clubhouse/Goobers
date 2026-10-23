package chaos

import "github.com/goobers/goobers/test/ha/workerscenarios"

// Scenarios is the HA chaos scenario set. Pending references are the open PRs
// whose components each scenario exercises; when one merges the scenario gets
// a Run body and drops its Pending field in the same change.
func Scenarios() []Scenario {
	return []Scenario{
		{ID: "CHAOS-4", Description: "CNPG primary failover: every 2xx write survives, retries dedup, one controller acquires after failover (lease library merged; needs the controller role)", Pending: "pending #7850"},
		{ID: "CHAOS-6", Description: "stage pod loses the API mid-write: same key exactly once, exhaustion is infra", Pending: "pending #7850"},
		{ID: "CHAOS-9", Description: "webhook redelivered concurrently: one source receipt, same acceptance", Pending: "pending #7850"},
		{ID: "CHAOS-13-CHILD", Description: "worker evicted while a generated child is mid side-effecting stage: the child execution and stage pod are adopted, no second admission or attempt charge (contract: test/ha/childscenarios; needs the child-plane composition)", Pending: "pending #7850"},
		{ID: "CHAOS-10", Description: "control restarts at :59.9, 20x jittered: one acceptance per cursor advance", Pending: "pending #7850"},
		{ID: workerscenarios.Chaos13, Description: "evict the owning worker mid side-effecting stage: adopt the same pod without charging an attempt", Pending: "pending #7927"},
		{ID: "RESTART-RTO", Description: "admission resumes within 60s of a rollout, p95 of 20 (reported, not asserted)", Pending: "pending #7850"},
		{ID: "NODEKILL-RTO", Description: "node-kill recovery within 2.5 min (reported, not asserted)", Pending: "pending #7850"},
		{ID: workerscenarios.Upgrade11, Description: "hold a run across a rainbow rollout: duplicate resolution applies once on the new build", Pending: "pending #7927"},
	}
}
