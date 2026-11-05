//go:build cloudintegration

package schedentries

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudclaims "github.com/goobers/goobers/internal/cloud/claims"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
)

type pinnedGenerations struct{}

func (pinnedGenerations) Acquire(context.Context, string) (childworkflow.Generation, error) {
	return childworkflow.Generation{Release: func() error { return nil }}, nil
}

// epoch is one controller incarnation: a lease handle, its fenced stores and the
// scheduler a leader builds from them.
type epoch struct {
	handle    *lease.Handle
	scheduler *localscheduler.Scheduler
}

func controllerPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := admin.Config().Copy()
	config.MaxConns = lease.MaxFencedConnections
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newEpoch(t *testing.T, pool *pgxpool.Pool, manager *lease.Manager, handle *lease.Handle, entries []localscheduler.WorkflowEntry, log journal.InstanceStore) *epoch {
	t.Helper()
	store, err := acceptance.New(pool, pgtest.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	store.BindFence(manager, handle)
	claims, err := cloudclaims.New(pool, pgtest.InstanceID, manager, handle)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := NewSources(Deps{Queue: store, Generations: pinnedGenerations{}, Build: func(context.Context, startintent.Target) (startintent.Prepared, error) {
		return startintent.Prepared{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &epoch{handle: handle, scheduler: localscheduler.New(entries, log, localscheduler.WithSourceQueue(sources), localscheduler.WithSchedulerState(claims))}
}

func occurrences(t *testing.T, admin *pgxpool.Pool) []string {
	t.Helper()
	rows, err := admin.Query(t.Context(), `SELECT payload FROM control.acceptance ORDER BY accepted_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		envelope, err := startintent.Parse(payload)
		if err != nil || envelope.Source == nil {
			t.Fatalf("accepted payload %q: %v", payload, err)
		}
		out = append(out, envelope.Source.ScheduledFrom.UTC().Format(time.RFC3339)+"->"+envelope.Source.ScheduledAt.UTC().Format(time.RFC3339))
	}
	sort.Strings(out)
	return out
}

// TestCronFiresExactlyOnceAcrossLeaseTakeovers drives the real scheduler tick
// over the real PG acceptance store across two lease takeovers, including a
// deposed leader that ticks after losing the lease. No occurrence may be
// accepted twice or lost.
func TestCronFiresExactlyOnceAcrossLeaseTakeovers(t *testing.T) {
	for name, catchUpAll := range map[string]bool{"coalesce": false, "catch up all": true} {
		t.Run(name, func(t *testing.T) {
			admin := pgtest.Pool(t, "schedentries")
			pool := controllerPool(t, admin)
			cfg, set := fixture(apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "@every 1m"})
			cfg.Timezone = ""
			set.Workflows[0].Spec.ScheduleCatchUp = map[bool]string{true: "all", false: ""}[catchUpAll]
			entries := build(t, cfg, set)
			log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })

			manager := pgtest.Manager(t, pool, "replica-1", 0)
			first := newEpoch(t, pool, manager, pgtest.Acquire(t, manager), entries, log)
			base := time.Now().UTC().Truncate(time.Minute)
			if err := first.scheduler.ReconcileAll(nil, base); err != nil {
				t.Fatal(err)
			}
			first.scheduler.Tick(t.Context(), base.Add(time.Minute))
			if got := len(occurrences(t, admin)); got != 1 {
				t.Fatalf("first leader accepted %d occurrences, want 1", got)
			}

			// Takeover 1. The new leader replays the same tick: nothing new.
			secondManager, secondHandle := takeover(t, pool, admin, "replica-2")
			second := newEpoch(t, pool, secondManager, secondHandle, entries, log)
			if err := second.scheduler.ReconcileAll(nil, base.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			second.scheduler.Tick(t.Context(), base.Add(time.Minute))
			if got := len(occurrences(t, admin)); got != 1 {
				t.Fatalf("replayed tick after takeover accepted a duplicate: %d", got)
			}

			// The deposed leader ticks into the next occurrence mid-flight: its fenced
			// accept must be refused, and the new leader then accepts it exactly once.
			first.scheduler.Tick(t.Context(), base.Add(2*time.Minute))
			if got := len(occurrences(t, admin)); got != 1 {
				t.Fatalf("a deposed leader accepted an occurrence: %d", got)
			}
			second.scheduler.Tick(t.Context(), base.Add(2*time.Minute))
			if got := len(occurrences(t, admin)); got != 2 {
				t.Fatalf("new leader accepted %d occurrences after the deposed leader was refused, want 2", got)
			}

			// Takeover 2, then a long gap: the new leader replays, then catches up.
			thirdManager, thirdHandle := takeover(t, pool, admin, "replica-3")
			third := newEpoch(t, pool, thirdManager, thirdHandle, entries, log)
			if err := third.scheduler.ReconcileAll(nil, base.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			third.scheduler.Tick(t.Context(), base.Add(2*time.Minute))
			if got := len(occurrences(t, admin)); got != 2 {
				t.Fatalf("replay after the second takeover accepted a duplicate: %d", got)
			}
			third.scheduler.Tick(t.Context(), base.Add(5*time.Minute))
			got := occurrences(t, admin)
			// Coalescing folds the three missed minutes into one catch-up worker;
			// catch-up-all accepts each of them independently. Either way no
			// occurrence repeats and none is lost.
			want := 3
			if catchUpAll {
				want = 5
			}
			if len(got) != want {
				t.Fatalf("occurrences = %v (%d), want %d", got, len(got), want)
			}
			for i := 1; i < len(got); i++ {
				if got[i] == got[i-1] {
					t.Fatalf("occurrence accepted twice: %s", got[i])
				}
			}
		})
	}
}

// takeover expires the controller heartbeat on the database clock and returns a
// manager that has acquired the lease at a higher epoch.
func takeover(t *testing.T, pool, admin *pgxpool.Pool, holder string) (*lease.Manager, *lease.Handle) {
	t.Helper()
	successor := pgtest.Manager(t, pool, holder, 0)
	return successor, pgtest.Takeover(t, admin, successor)
}
