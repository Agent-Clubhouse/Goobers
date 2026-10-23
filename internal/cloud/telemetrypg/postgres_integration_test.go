//go:build cloudintegration

package telemetrypg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetryclient"
)

func TestDefectNominationAggregatesParity(t *testing.T) {
	store, _ := telemetryTestStore(t, "controller")
	now := time.Now().UTC()
	findings := []struct {
		aggregate telemetryclient.Aggregate
		finding   telemetryclient.Finding
	}{
		{telemetryclient.AggregateStageFailureRate, telemetryclient.Finding{
			Kind: "stage-failure-rate", Subject: "build",
			Metrics: map[string]float64{
				"failureRate": 0.5, "totalAttempts": 10, "failedAttempts": 5,
			},
			Threshold:   0.3,
			FlaggedRuns: []telemetryclient.JournalPointer{{RunID: "run-stage", Seq: 4}},
		}},
		{telemetryclient.AggregateGateNoise, telemetryclient.Finding{
			Kind: "gate-repass-churn", Subject: "review",
			Metrics: map[string]float64{
				"escalationRate": 0.4, "totalEvaluations": 10, "escalatedCount": 4,
			},
			Threshold:   0.2,
			FlaggedRuns: []telemetryclient.JournalPointer{{RunID: "run-gate", Seq: 8}},
		}},
		{telemetryclient.AggregateCreditAssignment, telemetryclient.Finding{
			Kind: "credit-assignment", Subject: "stage:implement",
			Metrics:     map[string]float64{"runs": 8, "failureShare": 0.75},
			Threshold:   0.3,
			FlaggedRuns: []telemetryclient.JournalPointer{{RunID: "run-credit", Seq: 12}},
		}},
		{telemetryclient.AggregateErrorSignature, telemetryclient.Finding{
			Kind: "error-signature", Subject: "stage_timeout",
			Metrics: map[string]float64{"count": 6}, Threshold: 5,
			ErrorClass:  "timeout",
			FlaggedRuns: []telemetryclient.JournalPointer{{RunID: "run-error", Seq: 16}},
		}},
	}
	batch := Batch{
		Consumer: "defect-rollup",
		Current:  journal.StreamCursor{Stream: journal.StreamScheduler},
		NextSeq:  20,
		Milestones: Milestones{
			InitCompletedAt: now.Add(-time.Hour),
			FirstPROpenAt:   now.Add(-30 * time.Minute),
		},
	}
	for _, fixture := range findings {
		batch.Aggregates = append(batch.Aggregates, AggregateRow{
			Gaggle: "g", Workflow: "w", Aggregate: fixture.aggregate,
			ObservedAt: now, Finding: fixture.finding,
		})
	}
	if err := store.Apply(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(t.Context(), batch); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	conflict := batch
	conflict.Aggregates = append([]AggregateRow(nil), batch.Aggregates...)
	conflict.Aggregates[0].Finding = batch.Aggregates[0].Finding
	conflict.Aggregates[0].Finding.Metrics = map[string]float64{
		"failureRate": 0.9, "totalAttempts": 10, "failedAttempts": 9,
	}
	if err := store.Apply(t.Context(), conflict); !errors.Is(
		err, journal.ErrStreamCursorConflict) {
		t.Fatalf("changed replay = %v, want cursor conflict", err)
	}
	response, err := store.DefectAggregates(t.Context(),
		httpapi.TelemetryDefectAggregateRequest{
			Gaggle: "g", Workflow: "w", Since: now.Add(-24 * time.Hour),
			Aggregates: []telemetryclient.Aggregate{
				telemetryclient.AggregateStageFailureRate,
				telemetryclient.AggregateGateNoise,
				telemetryclient.AggregateCreditAssignment,
				telemetryclient.AggregateErrorSignature,
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]telemetryclient.Finding, 0, len(findings))
	for _, fixture := range findings {
		want = append(want, fixture.finding)
	}
	// The PG boundary normalizes every error signature exactly as the local
	// telemetry plane does.
	want[3].Subject, want[3].Signature =
		telemetryclient.NormalizeErrorSignature(want[3].Subject)
	sortFindings(want)
	if !reflect.DeepEqual(response.Findings, want) {
		gotJSON, _ := json.Marshal(response.Findings)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("PG findings=%s local wire findings=%s", gotJSON, wantJSON)
	}
	metric, err := store.TimeToFirstPR(t.Context())
	if err != nil || metric.Milliseconds == nil ||
		time.Duration(*metric.Milliseconds)*time.Millisecond != 30*time.Minute {
		t.Fatalf("first-success metric = %+v %v", metric, err)
	}
}

func TestTelemetryCursorMonotonicAndFenced(t *testing.T) {
	old, pool := telemetryTestStore(t, "old")
	now := time.Now().UTC()
	first := Batch{
		Consumer:   "rollup",
		Current:    journal.StreamCursor{Stream: journal.StreamScheduler},
		NextSeq:    5,
		Milestones: Milestones{InitCompletedAt: now},
	}
	if err := old.Apply(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	stale := first
	stale.NextSeq = 6
	if err := old.Apply(t.Context(), stale); !errors.Is(err, journal.ErrStreamCursorConflict) {
		t.Fatalf("stale cursor write = %v", err)
	}
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := telemetrySuccessor(t, pool, "current")
	advance := Batch{
		Consumer:   "rollup",
		Current:    journal.StreamCursor{Stream: journal.StreamScheduler, Seq: 5},
		NextSeq:    6,
		Milestones: Milestones{InitCompletedAt: now.Add(-time.Hour)},
	}
	if err := current.Apply(t.Context(), advance); err != nil {
		t.Fatal(err)
	}
	if err := old.AdvanceStreamCursor(t.Context(), "rollup",
		journal.StreamCursor{Stream: journal.StreamScheduler, Seq: 6}, 7,
	); !errors.Is(err, cloudlease.ErrStale) {
		t.Fatalf("stale owner cursor write = %v", err)
	}
	cursor, err := current.LoadStreamCursor(
		t.Context(), "rollup", journal.StreamScheduler)
	if err != nil || cursor.Seq != 6 {
		t.Fatalf("cursor = %+v %v", cursor, err)
	}
	metric, err := current.TimeToFirstPR(t.Context())
	if err != nil || metric.InitCompletedAt == nil ||
		!metric.InitCompletedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("monotonic earlier milestone = %+v %v", metric, err)
	}
}

func TestRepeatedAggregateKeyLastWins(t *testing.T) {
	store, _ := telemetryTestStore(t, "repeated-key")
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := telemetryclient.Finding{
		Kind: "stage-failure-rate", Subject: "build",
		Metrics: map[string]float64{"failureRate": 0.5, "totalAttempts": 4},
	}
	last := telemetryclient.Finding{
		Kind: "stage-failure-rate", Subject: "build",
		Metrics: map[string]float64{"failureRate": 0.6, "totalAttempts": 5},
	}
	batch := Batch{
		Consumer: "rollup",
		Current:  journal.StreamCursor{Stream: journal.StreamScheduler},
		NextSeq:  10,
		Aggregates: []AggregateRow{
			{Gaggle: "g", Workflow: "w", Aggregate: telemetryclient.AggregateStageFailureRate, ObservedAt: now, Finding: first},
			{Gaggle: "g", Workflow: "w", Aggregate: telemetryclient.AggregateStageFailureRate, ObservedAt: now.Add(time.Second), Finding: last},
		},
	}
	if err := store.Apply(t.Context(), batch); err != nil {
		t.Fatalf("Apply repeated aggregate key: %v", err)
	}
	response, err := store.DefectAggregates(t.Context(),
		httpapi.TelemetryDefectAggregateRequest{
			Gaggle: "g", Workflow: "w", Since: now.Add(-time.Minute),
			Aggregates: []telemetryclient.Aggregate{telemetryclient.AggregateStageFailureRate},
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Findings) != 1 || !reflect.DeepEqual(response.Findings[0], last) {
		t.Fatalf("findings = %+v, want last row %+v", response.Findings, last)
	}
}

func TestReplayFingerprintNormalizesTimestamps(t *testing.T) {
	store, _ := telemetryTestStore(t, "timestamp-replay")
	now := time.Now().UTC().Truncate(time.Microsecond)
	batch := Batch{
		Consumer: "rollup",
		Current:  journal.StreamCursor{Stream: journal.StreamScheduler},
		NextSeq:  3,
		Milestones: Milestones{
			InitCompletedAt: now,
			FirstPROpenAt:   now.Add(time.Second),
		},
		Aggregates: []AggregateRow{{
			Gaggle: "g", Aggregate: telemetryclient.AggregateStageFailureRate,
			ObservedAt: now,
			Finding: telemetryclient.Finding{
				Kind: "stage-failure-rate", Subject: "build",
				Metrics: map[string]float64{"failureRate": 0.5, "totalAttempts": 4},
			},
		}},
	}
	if err := store.Apply(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	replay := batch
	location := time.FixedZone("review", 3600)
	replay.Aggregates = append([]AggregateRow(nil), batch.Aggregates...)
	replay.Aggregates[0].ObservedAt = now.In(location).Add(500 * time.Nanosecond)
	replay.Milestones.InitCompletedAt = now.In(location).Add(500 * time.Nanosecond)
	replay.Milestones.FirstPROpenAt = now.Add(time.Second).In(location).Add(500 * time.Nanosecond)
	if err := store.Apply(t.Context(), replay); err != nil {
		t.Fatalf("semantically identical timestamp replay: %v", err)
	}
}

func sortFindings(findings []telemetryclient.Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Subject < findings[j].Subject
	})
}

func telemetryTestStore(
	t *testing.T,
	holder string,
) (*Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	database := "telemetry_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	identifier := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(t.Context(),
		"DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = database
	config.MaxConns = cloudlease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := (migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "telemetry-test",
		InstanceID: "migration", Holder: "migration",
	}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := telemetrySuccessor(t, pool, holder)
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	return store, pool
}

func telemetrySuccessor(
	t *testing.T,
	pool *pgxpool.Pool,
	holder string,
) *Store {
	t.Helper()
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "test-instance",
		Name: cloudlease.ControllerName, Holder: holder,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(pool, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	store.BindFence(manager, handle)
	return store
}
