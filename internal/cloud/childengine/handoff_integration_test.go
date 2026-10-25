//go:build cloudintegration

package childengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestAuthoritiesRecoverStoppedHandoffThroughControllerRole(t *testing.T) {
	f := newChildWorkflowFamily(t)
	run, origin := f.parent.pins.RunID, f.parent.origin
	started, err := f.service().StartChildWorkflow(t.Context(), f.grant.BearerToken, run, "call-1", []byte(generatedChild))
	if err != nil {
		t.Fatal(err)
	}
	request := engine.ChildHandoffObservation{ChildAuthorityRelease: engine.ChildAuthorityRelease{RunID: run, Origin: origin, GrantID: f.grant.GrantID}}
	owner := f.authorities()
	first, err := owner.ObserveChildHandoff(t.Context(), request)
	if err != nil || first == nil || first.AcceptanceID != started.AcceptanceID {
		t.Fatalf("observe=%+v %v", first, err)
	}
	request.Finish, request.Stopped = true, true
	sealed, err := owner.ObserveChildHandoff(t.Context(), request)
	if err != nil || sealed == nil || *sealed != *first {
		t.Fatalf("seal=%+v %v", sealed, err)
	}
	// The activity completion is lost. Even an expired, revoked grant must
	// recover the identical receipt without restoring its bearer authority.
	if _, err := f.pool.Exec(t.Context(), `UPDATE control.child_authority SET expires_at=clock_timestamp()-interval '1 second' WHERE instance_id=$1`, testInstance); err != nil {
		t.Fatal(err)
	}
	recovered, err := owner.AcquireChildAuthority(t.Context(), engine.ChildAuthorityRequest{RunID: run, Origin: origin, Endpoint: podEndpoint})
	if err != nil || recovered.StoppedHandoff == nil || *recovered.StoppedHandoff != *first || recovered.BearerToken != "" {
		t.Fatalf("recovery=%+v %v", recovered.StoppedHandoff, err)
	}
	again, err := owner.ObserveChildHandoff(t.Context(), request)
	if err != nil || again == nil || *again != *first {
		t.Fatalf("reobserve=%+v %v", again, err)
	}
	// Exercise ON CONFLICT through the runtime role too: a genuinely new
	// attempt may bind a new grant, and must never inherit the old stop receipt.
	bound, err := f.controller.ChildAuthority(t.Context(), triggerqueue.ChildParent{Gaggle: f.parent.pins.Gaggle, ParentRunID: run}, origin.StageOccurrence)
	if err != nil {
		t.Fatal(err)
	}
	next := bound
	next.GrantID, next.AttemptID, next.Revoked, next.ExpiresAt = "next-grant", "next-attempt", false, time.Now().Add(time.Hour)
	if err := f.controller.BindChildAuthority(t.Context(), next, bound.GrantID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if data, err := f.controller.RecoverChildHandoff(t.Context(), next); err != nil || len(data) != 0 {
		t.Fatalf("new attempt inherited receipt: %s %v", data, err)
	}
}

type handoffRaceQueue struct {
	*acceptance.Store
	beforeConfirm func()
}

func (q handoffRaceQueue) ConfirmChildHandoff(ctx context.Context, grant triggerqueue.ChildAuthority, child *triggerqueue.ChildRecord, receipt []byte, finish, stopped bool) error {
	q.beforeConfirm()
	return q.Store.ConfirmChildHandoff(ctx, grant, child, receipt, finish, stopped)
}

func TestObserveChildHandoffRechecksControllerFenceAfterSourceValidation(t *testing.T) {
	f := newChildWorkflowFamily(t)
	if _, err := f.service().StartChildWorkflow(t.Context(), f.grant.BearerToken, f.parent.pins.RunID, "call-1", []byte(generatedChild)); err != nil {
		t.Fatal(err)
	}
	owner := f.authorities()
	owner.Queue = handoffRaceQueue{Store: f.controller, beforeConfirm: func() {
		if err := f.manager.Release(t.Context(), f.handle); err != nil {
			t.Fatal(err)
		}
		f.takeOver(t, "controller-2")
	}}
	request := engine.ChildHandoffObservation{ChildAuthorityRelease: engine.ChildAuthorityRelease{RunID: f.parent.pins.RunID, Origin: f.parent.origin, GrantID: f.grant.GrantID}, Finish: true, Stopped: true}
	if h, err := owner.ObserveChildHandoff(t.Context(), request); h != nil || !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("stale controller delivered handoff: %+v %v", h, err)
	}
	var sealed bool
	if err := f.pool.QueryRow(t.Context(), `SELECT stopped_handoff IS NOT NULL FROM control.child_authority WHERE instance_id=$1`, testInstance).Scan(&sealed); err != nil || sealed {
		t.Fatalf("stale controller sealed=%v %v", sealed, err)
	}
}

func TestObserveChildHandoffClosesAcceptanceExitRace(t *testing.T) {
	f := newChildWorkflowFamily(t)
	owner := f.authorities()
	owner.Queue = handoffRaceQueue{Store: f.controller, beforeConfirm: func() {
		if _, err := f.service().StartChildWorkflow(t.Context(), f.grant.BearerToken, f.parent.pins.RunID, "call-1", []byte(generatedChild)); err != nil {
			t.Fatal(err)
		}
	}}
	request := engine.ChildHandoffObservation{ChildAuthorityRelease: engine.ChildAuthorityRelease{RunID: f.parent.pins.RunID, Origin: f.parent.origin, GrantID: f.grant.GrantID}, Finish: true, Stopped: true}
	if h, err := owner.ObserveChildHandoff(t.Context(), request); h != nil || !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("changed snapshot not retried: %+v %v", h, err)
	}
	owner = f.authorities()
	if h, err := owner.ObserveChildHandoff(t.Context(), request); h == nil || err != nil {
		t.Fatalf("accepted child missed on final retry: %+v %v", h, err)
	}
}

func TestAuthoritiesNeverRecoverAnUnprovedStop(t *testing.T) {
	f := newChildWorkflowFamily(t)
	if _, err := f.service().StartChildWorkflow(t.Context(), f.grant.BearerToken, f.parent.pins.RunID, "call-1", []byte(generatedChild)); err != nil {
		t.Fatal(err)
	}
	owner := f.authorities()
	request := engine.ChildHandoffObservation{ChildAuthorityRelease: engine.ChildAuthorityRelease{RunID: f.parent.pins.RunID, Origin: f.parent.origin, GrantID: f.grant.GrantID}, Finish: true}
	if h, err := owner.ObserveChildHandoff(t.Context(), request); h == nil || err != nil {
		t.Fatalf("observe=%+v %v", h, err)
	}
	if grant, err := owner.AcquireChildAuthority(t.Context(), engine.ChildAuthorityRequest{RunID: request.RunID, Origin: request.Origin, Endpoint: podEndpoint}); grant.StoppedHandoff != nil || !errors.Is(err, engine.ErrChildCustodyRefused) {
		t.Fatalf("unproved stop recovered: %+v %v", grant.StoppedHandoff, err)
	}
}
