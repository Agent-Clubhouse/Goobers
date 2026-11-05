//go:build cloudintegration

package childengine

import (
	"net/http"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildQualificationRetention(t *testing.T) {
	h := newQualificationHarness(t)
	barrier := h.InstallHTTPBarrier(func(request *http.Request) bool {
		return request.Method == http.MethodPost && request.URL.Path == apicontract.EngineChildOriginPath
	})
	run := h.StartParent()
	if err := barrier.Wait(h.ctx); err != nil {
		t.Fatal(err)
	}
	// Hold the committed stage-origin acknowledgement before DispatchStage
	// can acquire authority. The executor's parent-start phase is too late:
	// production dispatch binds its grant before launching that executor.
	qualificationNoChildren(h)
	var grants int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_authority WHERE instance_id=$1 AND parent_run_id=$2`, testInstance, run.GetID()).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("grant overtook retention boundary: %d (%v)", grants, err)
	}
	// Age only retention timestamps; Temporal liveness, pins and the sweep are real.
	before, err := h.epoch.store.RunPinsOf(h.ctx, run.GetID())
	if err != nil {
		t.Fatal(err)
	}
	age := (triggerqueue.ReplayRetention + time.Hour).Seconds()
	qualificationAge(h, `UPDATE control.acceptance SET accepted_at=clock_timestamp()-$3*interval '1 second',
		finished_at=clock_timestamp()-$3*interval '1 second' WHERE instance_id=$1 AND id=$2`, testInstance, before.AcceptanceID, age)
	qualificationAge(h, `UPDATE control.run_pin SET created_at=clock_timestamp()-$3*interval '1 second'
		WHERE instance_id=$1 AND run_id=$2`, testInstance, run.GetID(), age)
	if err := h.epoch.store.Prune(h.ctx); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.acceptance WHERE instance_id=$1 AND id=$2`, testInstance, before.AcceptanceID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("old acceptance remains: %d (%v)", remaining, err)
	}
	sweep, err := h.epoch.store.SweepRunPins(h.ctx, epoch.TemporalConfirmClosed(h.temporal.Client()), "", time.Minute)
	if err != nil || sweep.Marked != 0 || sweep.Deleted != 0 || sweep.Halted != nil || len(sweep.Unconfirmed) != 0 || len(sweep.Checked) != 1 || sweep.Checked[0] != run.GetID() {
		t.Fatalf("live parent retention sweep: %+v (%v)", sweep, err)
	}
	after, err := h.epoch.store.RunPinsOf(h.ctx, run.GetID())
	if err != nil || after != before {
		t.Fatalf("retention changed live parent pins: %+v (%v)", after, err)
	}
	var terminal *time.Time
	if err := h.pool.QueryRow(h.ctx, `SELECT terminal_at FROM control.run_pin WHERE instance_id=$1 AND run_id=$2`, testInstance, run.GetID()).Scan(&terminal); err != nil || terminal != nil {
		t.Fatalf("live parent pin marked terminal: %v (%v)", terminal, err)
	}
	barrier.Release()
	qualificationChildCompleted(h, run)
	h.assertProductionChildHandoff(run)
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_authority WHERE instance_id=$1 AND parent_run_id=$2`, testInstance, run.GetID()).Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("grant binding after acceptance pruning: %d (%v)", grants, err)
	}
}
