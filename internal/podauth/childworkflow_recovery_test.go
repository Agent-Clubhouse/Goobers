package podauth

import (
	"strings"
	"testing"
	"time"
)

func TestRecoverChildGrantKeepsNonceAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	key := grantKey(t, 7, &now)
	token, grant, err := key.MintChildWorkflowGrant(childGrantFixture(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := key.RecoverChildWorkflowGrant(grant)
	if err != nil || recovered != token {
		t.Fatal("delivery recovery changed token", err)
	}
	for _, mutate := range []func(*ChildWorkflowGrant){
		func(g *ChildWorkflowGrant) { g.ID = "bad" },
		func(g *ChildWorkflowGrant) { g.ID = strings.Repeat("a", 100) },
		func(g *ChildWorkflowGrant) { g.ExpiresAt = now.Add(-time.Second) },
		func(g *ChildWorkflowGrant) { g.ExpiresAt = now.Add(25 * time.Hour).Truncate(time.Second) },
		func(g *ChildWorkflowGrant) { g.ExpiresAt = g.ExpiresAt.Add(time.Nanosecond) },
		func(g *ChildWorkflowGrant) { g.PolicyDigest = "bad" },
	} {
		bad := grant
		mutate(&bad)
		if _, err := key.RecoverChildWorkflowGrant(bad); err == nil {
			t.Fatal("invalid retained grant re-signed")
		}
	}
	now = grant.ExpiresAt
	if _, err := key.RecoverChildWorkflowGrant(grant); err == nil {
		t.Fatal("expired grant re-signed")
	}
}
