package localscheduler

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// TestAdmitOpenPRCapOnADORepository is ADO-N30: a refresher over an Azure
// DevOps repository counts that repo's run-branch PRs, so Admit blocks at the
// cap exactly as it does on GitHub. The human-parked label is matched
// regardless of casing, because ADO keeps the casing of whoever created the
// label first.
func TestAdmitOpenPRCapOnADORepository(t *testing.T) {
	lister := &fakeOpenPRLister{
		heads: []string{
			"goobers/implementation/run-1",
			"goobers/implementation/run-2",
			"goobers/implementation/run-3",
		},
		labels: map[string][]string{
			"goobers/implementation/run-3": {"Goobers:Merge-Escalated"},
		},
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "example-project", Name: "web"}
	r := NewOpenPRRefresher(lister, repo, time.Hour, []string{"goobers:merge-escalated"}, nil)
	r.pollOnce(context.Background())
	if n, known := r.OpenPRCount("", "implementation"); !known || n != 2 {
		t.Fatalf("count=%d known=%v, want 2/true (parked PR excluded case-insensitively)", n, known)
	}

	c := NewConditions()
	c.SetOpenPRCounter(r)
	now := time.Now()
	atCap := apiv1.ReadinessConditions{MaxConcurrentRuns: 10, MaxOpenPRs: 2}
	if ok, reason := c.Admit("implementation", atCap, now); ok || reason != ReasonOpenPRCap {
		t.Fatalf("ok=%v reason=%q, want blocked with %q", ok, reason, ReasonOpenPRCap)
	}
	belowCap := apiv1.ReadinessConditions{MaxConcurrentRuns: 10, MaxOpenPRs: 3}
	if ok, reason := c.Admit("implementation", belowCap, now); !ok {
		t.Fatalf("ok=%v reason=%q, want admitted below the cap", ok, reason)
	}
}
