//go:build cloudintegration

package acceptance

import (
	"errors"
	"github.com/goobers/goobers/internal/triggerqueue"
	"testing"
	"time"
)

func TestPGFullStoreStillReplays(t *testing.T) {
	s, p := testStore(t, 1)
	ctx := t.Context()
	now := time.Now().UTC()
	r, _, err := s.Accept(ctx, "original", "actor", []byte("body"), now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Exec(ctx, `INSERT INTO control.acceptance(instance_id,id,idem_key,actor,fingerprint,payload,state,run_id,accepted_at) SELECT 'test-instance','trigger-fill-'||n,'fill-'||n,'actor',decode('00','hex'),decode('78','hex'),'accepted','fill-'||n,$1 FROM generate_series(1,$2) n`, now, triggerqueue.MaxRecords-1)
	if err != nil {
		t.Fatal(err)
	}
	got, repeated, err := s.Accept(ctx, "original", "actor", []byte("body"), now)
	if err != nil || !repeated || got.ID != r.ID {
		t.Errorf("full store exact retry: id=%q repeated=%v err=%v", got.ID, repeated, err)
	}
	_, _, err = s.Accept(ctx, "original", "actor", []byte("different"), now)
	if !errors.Is(err, triggerqueue.ErrConflict) {
		t.Errorf("full store mismatching retry: %v", err)
	}
}
func TestPGStaleSourceWritesAreFenced(t *testing.T) {
	for _, action := range []string{"accept-source", "capture-demand", "observe-demand", "occurrences"} {
		t.Run(action, func(t *testing.T) {
			old, p := testStore(t, 1)
			ctx := t.Context()
			base := time.Now().UTC().Truncate(time.Second)
			if _, _, err := old.SourceCursorRevision(ctx, "scope", "revision", base, base, false); err != nil {
				t.Fatal(err)
			}
			advance := triggerqueue.SourceAdvance{Scope: "scope", Revision: "revision", Before: base, After: base.Add(time.Minute)}
			if action == "observe-demand" {
				if err := old.CaptureScheduleDemand(ctx, triggerqueue.ScheduleDemandCapture{ID: "existing", Payload: []byte("body"), AcceptedAt: base, Advance: advance}); err != nil {
					t.Fatal(err)
				}
			}
			if err := old.manager.Release(ctx, old.handle); err != nil {
				t.Fatal(err)
			}
			_ = successorStore(t, p)
			var err error
			switch action {
			case "accept-source":
				_, _, err = old.AcceptSource(ctx, triggerqueue.SourceBatch{Key: "late", Actor: "scheduler", Fingerprint: "f", Advance: &advance, Starts: []triggerqueue.SourceStart{{Payload: []byte("body")}}}, base)
			case "capture-demand":
				err = old.CaptureScheduleDemand(ctx, triggerqueue.ScheduleDemandCapture{ID: "late", Payload: []byte("body"), AcceptedAt: base, Advance: advance})
			case "observe-demand":
				err = old.ObserveScheduleDemand(ctx, "existing", 0)
			case "occurrences":
				_, _, err = old.SourceOccurrences(ctx, "scope", "revision", 2)
			}
			if err == nil {
				t.Errorf("stale owner committed %s after real lease transfer", action)
			}
			var cursor time.Time
			var demands, accepts int
			if err := p.QueryRow(ctx, "SELECT cursor_at FROM control.source_cursor WHERE scope='scope'").Scan(&cursor); err != nil {
				t.Fatal(err)
			}
			if err := p.QueryRow(ctx, "SELECT count(*) FROM control.schedule_demand").Scan(&demands); err != nil {
				t.Fatal(err)
			}
			if err := p.QueryRow(ctx, "SELECT count(*) FROM control.acceptance").Scan(&accepts); err != nil {
				t.Fatal(err)
			}
			wantCursor, wantDemands := base, 0
			if action == "observe-demand" {
				wantCursor, wantDemands = advance.After, 1
			}
			if !cursor.Equal(wantCursor) || demands != wantDemands || accepts != 0 {
				t.Fatalf("stale mutation changed state: cursor=%v demands=%d acceptances=%d", cursor, demands, accepts)
			}
			var occurrences []time.Time
			if err := p.QueryRow(ctx, "SELECT occurrence_cursors FROM control.source_cursor WHERE scope='scope'").Scan(&occurrences); err != nil {
				t.Fatal(err)
			}
			if occurrences != nil {
				t.Fatal("stale occurrence initialization committed")
			}
		})
	}
}
