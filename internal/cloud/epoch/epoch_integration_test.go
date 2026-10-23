//go:build cloudintegration

package epoch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/cancelpg"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/terminalreportpg"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

func controllerPool(t *testing.T, owner *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := owner.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var current string
	if err := pool.QueryRow(t.Context(), "SELECT current_user").Scan(&current); err != nil || current != "goobers_controller" {
		t.Fatalf("current_user = %q, %v; want goobers_controller", current, err)
	}
	return pool
}

func effect(item string) terminalreport.Effect {
	return terminalreport.Effect{
		Kind:       terminalreport.WorkFailure,
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Name: "app"},
		ItemID:     item, RunID: "run-" + item, Stage: "ci", Count: 1,
	}
}

// TestTwoConsecutiveTakeoversLeaveNothingStranded: epoch 1 and epoch 2 each
// leave unfinished cancel receipts and undelivered terminal reports, and each
// holder dies before its successor adopts anything. Epoch 3's rebuild moves
// every row from BOTH older epochs, as goobers_controller, and a stale handle
// from epoch 1 can no longer adopt.
func TestTwoConsecutiveTakeoversLeaveNothingStranded(t *testing.T) {
	owner := pgtest.Pool(t, "epoch")
	controller := controllerPool(t, owner)

	firstManager := pgtest.Manager(t, controller, "first", 0)
	first := pgtest.Acquire(t, firstManager)
	cancelFirst, terminalFirst := cancelpg.Store{Fence: firstManager}, terminalreportpg.Store{Fence: firstManager}
	if _, fresh, err := cancelFirst.Begin(t.Context(), first, "cancel-1", "operator", []byte("cancel run-1")); err != nil || !fresh {
		t.Fatalf("epoch 1 cancel receipt: fresh=%v err=%v", fresh, err)
	}
	if _, err := terminalFirst.Publish(t.Context(), first, effect("1")); err != nil {
		t.Fatal(err)
	}

	secondManager := pgtest.Manager(t, controller, "second", 0)
	second := pgtest.Takeover(t, owner, secondManager)
	cancelSecond, terminalSecond := cancelpg.Store{Fence: secondManager}, terminalreportpg.Store{Fence: secondManager}
	if _, fresh, err := cancelSecond.Begin(t.Context(), second, "cancel-2", "operator", []byte("cancel run-2")); err != nil || !fresh {
		t.Fatalf("epoch 2 cancel receipt: fresh=%v err=%v", fresh, err)
	}
	if _, err := terminalSecond.Publish(t.Context(), second, effect("2")); err != nil {
		t.Fatal(err)
	}
	// The epoch-2 holder crashes mid-rebuild: it adopted nothing.

	thirdManager := pgtest.Manager(t, controller, "third", 0)
	third := pgtest.Takeover(t, owner, thirdManager)
	rec := &recorder{}
	claims := &fakeClaims{adopter: adopter{name: "claims", rec: rec}}
	services, err := Build(Config{
		Handle: third, Claims: claims,
		Cancel: cancelpg.Store{Fence: thirdManager}, Terminal: terminalreportpg.Store{Fence: thirdManager},
		Open:         fakeOpen{runs: map[string]bool{"run-1": true}, rec: rec},
		Observations: newConfigObservations(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := services.RebuildBeforeReap(t.Context()); err != nil {
		t.Fatal(err)
	}

	for _, table := range []struct{ name, query string }{
		{"cancel receipts", `SELECT owner_epoch FROM control.cancel_receipt WHERE instance_id=$1 AND result IS NULL`},
		{"terminal reports", `SELECT owner_epoch FROM control.terminal_report WHERE instance_id=$1 AND delivered_at IS NULL`},
	} {
		rows, err := owner.Query(t.Context(), table.query, pgtest.InstanceID)
		if err != nil {
			t.Fatal(err)
		}
		var epochs []int64
		for rows.Next() {
			var e int64
			if err := rows.Scan(&e); err != nil {
				t.Fatal(err)
			}
			epochs = append(epochs, e)
		}
		rows.Close()
		if len(epochs) != 2 || epochs[0] != third.Epoch || epochs[1] != third.Epoch {
			t.Fatalf("%s owners = %v, want both rows adopted from epochs 1 and 2 into %d", table.name, epochs, third.Epoch)
		}
	}
	if got := rec.list(); fmt.Sprint(got) != fmt.Sprint([]string{"adopt:claims@3", "open", "renew"}) {
		t.Fatalf("claims were not adopted into epoch 3 and renewed against the open runs: %v", got)
	}

	// The epoch-1 holder's stale handle can no longer adopt or write.
	if _, err := (terminalreportpg.Store{Fence: firstManager}).AdoptAll(t.Context(), first); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale epoch-1 adoption = %v, want ErrStale", err)
	}
	if !IsStale(lease.ErrStale) {
		t.Fatal("IsStale must classify a fenced-out handle")
	}
}

func newConfigObservations() Observations {
	return Observations{
		Restore: func(context.Context, []control.Observation) error { return nil },
		Capture: func(context.Context) ([]control.Observation, error) { return nil, nil },
	}
}
