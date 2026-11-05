//go:build cloudintegration

package claims

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

// seedSharedState exercises the new columns and grant inventory on the runtime
// pool under the real controller fence. No owner-role session writes the state.
func seedSharedState(t *testing.T, p *Plane, revoked bool) localscheduler.ClaimEntry {
	t.Helper()
	ctx := t.Context()
	if got, err := p.Acquire(ctx, request("42", "run-a")); err != nil || !got.Ok {
		t.Fatalf("acquire = %+v, %v", got, err)
	}
	owner, err := json.Marshal(sharedclaim.Owner{Instance: pgtest.InstanceID, Run: "run-a", Token: "incarnation"})
	if err != nil {
		t.Fatal(err)
	}
	err = p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		_, err := tx.Exec(ctx, `UPDATE control.claim
			SET shared_owner=$2, shared_deadline=expires_at,
			    shared_admission_pending=true, shared_revoked=$3
			WHERE instance_id=$1 AND run_id='run-a'`, pgtest.InstanceID, owner, revoked)
		if err != nil {
			return err
		}
		key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
		// Repeat to exercise ON CONFLICT EXCLUDED with controller SELECT grants.
		if err := recordGrant(ctx, tx, p.instanceID, key, epoch); err != nil {
			return err
		}
		return recordGrant(ctx, tx, p.instanceID, key, epoch)
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := p.selectEntries(ctx, p.pool, `run_id=$2`, "run-a")
	if err != nil || len(entries) != 1 {
		t.Fatalf("shared read = %+v, %v", entries, err)
	}
	entry := entries[0]
	if entry.SharedOwner.Token != "incarnation" || !entry.SharedDeadline.Equal(entry.ExpiresAt) ||
		!entry.SharedAdmissionPending || entry.SharedRevoked != revoked {
		t.Fatalf("lost shared state: %+v", entry)
	}
	return entry
}

func TestSharedStateSurvivesReleaseAndHistoryRetentionAsController(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		name := "ordinary"
		if revoked {
			name = "revoked"
		}
		t.Run(name, func(t *testing.T) {
			f := newPlaneFixture(t)
			p := f.leader(t, "a").plane
			entry := seedSharedState(t, p, revoked)
			ctx := t.Context()
			err := p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
				if _, err := tx.Exec(ctx, `UPDATE control.claim SET release_pending_at=clock_timestamp()
					WHERE instance_id=$1`, p.instanceID); err != nil {
					return err
				}
				ok, err := releaseToHistory(ctx, tx, p.instanceID, epoch, storedKey(entry), entry.RunID,
					"", false, true, claimrelease.Completed.Because("test", ""))
				if err == nil && !ok {
					t.Fatal("release did not move the shared row")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			listed, err := p.List(ctx, httpapi.ClaimListRequest{RunID: entry.RunID, Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
			if err != nil || len(listed.Entries) != 0 || len(listed.History) != 1 {
				t.Fatalf("released history = %+v, %v", listed, err)
			}
			history := listed.History[0]
			if history.SharedOwner != entry.SharedOwner || !history.SharedDeadline.Equal(entry.SharedDeadline) ||
				history.SharedAdmissionPending || history.ReleasePending || history.SharedRevoked != revoked {
				t.Fatalf("released shared metadata = %+v", history)
			}
			// Age history using fixture authority; reads and pruning remain runtime-role operations.
			if _, err := f.owner.Exec(ctx, `UPDATE control.claim_run_history SET released_at=clock_timestamp()-$2::interval
				WHERE instance_id=$1`, p.instanceID, 2*localscheduler.ClaimHistoryTTL); err != nil {
				t.Fatal(err)
			}
			err = p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
				return pruneHistory(ctx, tx, p.instanceID, epoch)
			})
			if err != nil {
				t.Fatal(err)
			}
			histories, err := queryHistory(ctx, p.pool, p.instanceID, `true`, historyNewestFirst)
			want := 0
			if revoked {
				want = 1
			}
			if err != nil || len(histories) != want {
				t.Fatalf("retained revoked=%v history = %+v, %v", revoked, histories, err)
			}
			var count int
			if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM control.claim_run_history WHERE instance_id=$1`, p.instanceID).Scan(&count); err != nil || count != want {
				t.Fatalf("claim_run_history count=%d, %v; want %d", count, err, want)
			}
		})
	}
}

func TestSharedCustodyCannotUseLocalTransitionsAsController(t *testing.T) {
	f := newPlaneFixture(t)
	r := f.leader(t, "a")
	p := r.plane
	entry := seedSharedState(t, p, false)
	ctx := t.Context()
	for _, run := range []string{"run-a", "run-b"} {
		if got, err := p.Acquire(ctx, request("42", run)); err != nil || got.Ok {
			t.Fatalf("local acquire = %+v, %v", got, err)
		}
	}
	if got, err := p.Renew(ctx, request("42", entry.RunID)); err != nil || got.Ok {
		t.Fatalf("local renew = %+v, %v", got, err)
	}
	if _, err := p.Release(ctx, request("42", entry.RunID)); planeCode(err) != "409 claim_release_pending" {
		t.Fatalf("local release = %v", err)
	}
	if got, err := p.RenewRuns(ctx, r.handle.Load(), map[string]bool{entry.RunID: true}); err != nil || len(got) != 0 {
		t.Fatalf("epoch renew = %+v, %v", got, err)
	}
	store, err := New(f.controller, p.instanceID, r.manager, r.handle.Load())
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := store.ClaimScoped(storedKey(entry), entry.RunID, entry.Workflow, time.Hour); err != nil || ok {
		t.Fatalf("scheduler claim = %v, %v", ok, err)
	}
	if err := store.ReleaseEntry(entry, entry.RunID, claimrelease.Completed.Because("test", "")); err == nil {
		t.Fatal("scheduler release discarded shared custody")
	}
	// A new leader preserves the remote incarnation; the old leader cannot write.
	next := f.takeover(t, "b")
	if _, err := next.plane.AdoptAll(ctx, next.handle.Load()); err != nil {
		t.Fatal(err)
	}
	if err := p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		return recordGrant(ctx, tx, p.instanceID, storedKey(entry), epoch)
	}); planeCode(err) != "503 recovering" {
		t.Fatalf("stale grant = %v", err)
	}
}
