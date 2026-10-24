//go:build cloudintegration

package claims

import (
	"testing"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/pgroles"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestSharedAdmissionPrunesExpiredHistoryAndKeepsRevocationsAsController(t *testing.T) {
	f := newPlaneFixture(t)
	p := sharedPlane(t, f, f.leader(t, "a"), &sharedRemote{})
	ctx := t.Context()
	var role string
	if err := p.pool.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != pgroles.Controller {
		t.Fatalf("runtime role=%s, %v", role, err)
	}
	for _, run := range []string{"ordinary", "revoked"} {
		req := request("42", run)
		if got, err := p.Acquire(ctx, req); err != nil || !got.Ok {
			t.Fatalf("%s shared acquire=%+v, %v", run, got, err)
		}
		if run == "revoked" {
			req.Reason = claimrelease.ForceReleased.Because("test", "administrator")
			req.Reason.Actor = "administrator"
		}
		if got, err := p.Release(ctx, req); err != nil || !got.Ok {
			t.Fatalf("%s shared release=%+v, %v", run, got, err)
		}
	}
	for _, table := range []string{"claim_history", "claim_run_history"} {
		// Only fixture aging uses owner authority. Admission, release, pruning,
		// and the physical-row assertions all use the runtime controller role.
		if _, err := f.owner.Exec(ctx, `UPDATE control.`+table+`
			SET released_at=clock_timestamp()-$2::interval WHERE instance_id=$1`,
			p.instanceID, 2*localscheduler.ClaimHistoryTTL); err != nil {
			t.Fatal(err)
		}
	}
	req := request("42", "next")
	if got, err := p.Acquire(ctx, req); err != nil || !got.Ok {
		t.Fatalf("next shared acquire=%+v, %v", got, err)
	}
	if got, err := p.Release(ctx, req); err != nil || !got.Ok {
		t.Fatalf("next shared release=%+v, %v", got, err)
	}
	for _, table := range []string{"claim_history", "claim_run_history"} {
		var ordinary, revoked int
		if err := p.pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE run_id='ordinary'),
			count(*) FILTER (WHERE run_id='revoked' AND shared_revoked)
			FROM control.`+table+` WHERE instance_id=$1`, p.instanceID).Scan(&ordinary, &revoked); err != nil || ordinary != 0 || revoked != 1 {
			t.Fatalf("%s ordinary=%d revoked=%d, %v", table, ordinary, revoked, err)
		}
	}
	if got, err := p.Acquire(ctx, request("42", "revoked")); err == nil || got.Ok {
		t.Fatalf("expired revocation lost its denial=%+v, %v", got, err)
	}
}
