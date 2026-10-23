//go:build cloudintegration

package banditstore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/bandit/conformance"
	"github.com/goobers/goobers/internal/cloud/banditstore"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/journal"
)

func TestPostgresObservationStore(t *testing.T) {
	conformance.Run(t, func(t *testing.T) bandit.ObservationStore {
		_, _, s := acquireStore(t, testPool(t), "instance", "first")
		return s
	})
}

func TestObservationStoreFencesZombieAndRetriesLostResponse(t *testing.T) {
	pool := testPool(t)
	first, handle, s := acquireStore(t, pool, "instance", "first")
	r := conformance.Record("run-a", "build", 0, 1, 1)
	if err := s.Record(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	_, nextHandle, next := acquireStore(t, pool, "instance", "second")
	if err := next.Record(t.Context(), r); err != nil {
		t.Fatalf("lost-response retry across takeover: %v", err)
	}
	var epoch int64
	if err := pool.QueryRow(t.Context(), `SELECT owner_epoch FROM control.runner_bandit`).Scan(&epoch); err != nil || epoch != handle.Epoch {
		t.Fatalf("retry rewrote immutable outcome epoch: %d, %v", epoch, err)
	}
	r.Key.Attempt++
	if err := s.Record(t.Context(), r); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("zombie write = %v", err)
	}
	if _, err := s.Observations(t.Context(), "build"); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("zombie read = %v", err)
	}
	if err := next.Record(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	key, _, err := bandit.EncodeRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT owner_epoch FROM control.runner_bandit WHERE record_key=$1`, key).Scan(&epoch); err != nil || epoch != nextHandle.Epoch {
		t.Fatalf("successor epoch stamp: %d, %v", epoch, err)
	}
	got, err := next.Observations(t.Context(), "build")
	if err != nil || len(got) != 2 {
		t.Fatalf("takeover history = %v, %v", got, err)
	}
	_, _, isolated := acquireStore(t, pool, "other-instance", "first")
	if got, err := isolated.Observations(t.Context(), "build"); err != nil || len(got) != 0 {
		t.Fatalf("cross-instance sample leak = %v, %v", got, err)
	}
}

func TestJournalImportMatchesLocalSamplingAndIsIdempotent(t *testing.T) {
	_, _, cloud := acquireStore(t, testPool(t), "instance", "first")
	runsDir := observationJournals(t)
	want, err := bandit.LoadFileObservations(runsDir, "build")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := cloud.ImportLocalJournals(t.Context(), runsDir); err != nil || result.Imported != 6 || result.Existing != 0 {
		t.Fatalf("initial import = %+v, %v", result, err)
	}
	if result, err := cloud.ImportLocalJournals(t.Context(), runsDir); err != nil || result.Imported != 0 || result.Existing != 6 {
		t.Fatalf("repeated import = %+v, %v", result, err)
	}
	got, err := cloud.Observations(t.Context(), "build")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("cloud %v != local %v: %v", got, want, err)
	}
	// Historical sample identity is separate from a future native attempt,
	// even if its occurrence number happens to equal an old event sequence.
	if err := cloud.Record(t.Context(), conformance.Record("run-a", "build", 0, 2, 1)); err != nil {
		t.Fatal(err)
	}
	if got, err := cloud.Observations(t.Context(), "build"); err != nil || len(got) != 7 {
		t.Fatalf("native/import collision: %v, %v", got, err)
	}
}

func TestJournalImportResumesAfterPartialFailureAndTakeover(t *testing.T) {
	pool := testPool(t)
	first, handle, cloud := acquireStore(t, pool, "instance", "first")
	runsDir := observationJournals(t)
	if _, err := pool.Exec(t.Context(), `CREATE FUNCTION control.fail_bandit_import() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.payload#>>'{key,runId}'='run-b' THEN RAISE EXCEPTION 'injected import failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_bandit_import BEFORE INSERT ON control.runner_bandit FOR EACH ROW EXECUTE FUNCTION control.fail_bandit_import()`); err != nil {
		t.Fatal(err)
	}
	if result, err := cloud.ImportLocalJournals(t.Context(), runsDir); err == nil || result.Imported != 2 {
		t.Fatalf("partial import = %+v, %v", result, err)
	}
	if _, err := pool.Exec(t.Context(), `DROP TRIGGER fail_bandit_import ON control.runner_bandit`); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	_, _, next := acquireStore(t, pool, "instance", "second")
	if _, err := cloud.ImportLocalJournals(t.Context(), runsDir); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale import = %v", err)
	}
	if result, err := next.ImportLocalJournals(t.Context(), runsDir); err != nil || result.Imported != 4 || result.Existing != 2 {
		t.Fatalf("resumed import = %+v, %v", result, err)
	}
	if got, err := next.Observations(t.Context(), "build"); err != nil || len(got) != 6 {
		t.Fatalf("partial retry duplicated samples: %v, %v", got, err)
	}
}

func TestJournalImportRejectsChangedDurableEvent(t *testing.T) {
	_, _, cloud := acquireStore(t, testPool(t), "instance", "first")
	runsDir := observationJournals(t)
	path := filepath.Join(runsDir, "run-a", "events.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cloud.ImportLocalJournals(t.Context(), runsDir); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("import altered source journal: %v", err)
	}
	// A corrupt/replaced source reuses its durable event identity with another
	// valid outcome. Import must not overwrite the already committed fact.
	changed := bytes.Replace(before, []byte(`"arm":"control"`), []byte(`"arm":"different"`), 1)
	if bytes.Equal(before, changed) {
		t.Fatal("fixture did not contain the expected observation")
	}
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cloud.ImportLocalJournals(t.Context(), runsDir); !errors.Is(err, bandit.ErrObservationConflict) {
		t.Fatalf("changed event import = %v", err)
	}
	got, err := cloud.Observations(t.Context(), "build")
	if err != nil || len(got) != 6 || got[0].Arm != "control" {
		t.Fatalf("conflict changed committed history: %v, %v", got, err)
	}
}

func observationJournals(t *testing.T) string {
	t.Helper()
	runsDir := t.TempDir()
	// Equal assignment sequences deliberately exercise the backend tie-break.
	for _, runID := range []string{"run-c", "run-a", "run-b"} {
		run, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID, Workflow: "experiment", WorkflowVersion: 1, WorkflowDigest: "sha256:fixture", Gaggle: "test", Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		config := bandit.Config{Stage: "build"}
		for _, arm := range []string{"control", "treatment"} {
			o := conformance.Record(runID, "build", 0, 1, 1).Observation
			o.Arm = arm
			if err := config.RecordObservation(o, run); err != nil {
				t.Fatal(err)
			}
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return runsDir
}

func acquireStore(t *testing.T, pool *pgxpool.Pool, instance, holder string) (*lease.Manager, *lease.Handle, *banditstore.Store) {
	t.Helper()
	m, err := lease.New(lease.Config{Pool: pool, InstanceID: instance, Name: "controller", Holder: holder, TTL: time.Minute, SelfFenceAfter: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h, err := m.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s, err := banditstore.New(m, h)
	if err != nil {
		t.Fatal(err)
	}
	return m, h, s
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("bandit_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	config.MaxConns = lease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := migrate.Runner{Direct: pool, Catalog: catalog, BackupRef: "bandit-test", InstanceID: "migration", Holder: "test"}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool
}
