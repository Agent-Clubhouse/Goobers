package migrate

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestOldBinaryServesExpandSchema(t *testing.T) {
	oldCatalog := []Migration{{
		Version: 1, Kind: Expand, Name: "0001_base.expand.sql",
		BinaryVersion: "1.0.0", MinCompatibleBinary: "1.0.0",
	}}
	applied := []Applied{
		{Version: 1, Kind: Expand, BinaryVersion: "1.0.0", MinCompatibleBinary: "1.0.0"},
		{Version: 2, Kind: Expand, BinaryVersion: "2.0.0", MinCompatibleBinary: "1.0.0"},
	}
	if err := CheckCompatibility("1.0.0", applied, oldCatalog); err != nil {
		t.Fatalf("old binary rejected expand schema: %v", err)
	}
}

func TestMinCompatibleBinaryIsFirstNonReader(t *testing.T) {
	oldCatalog := []Migration{
		{Version: 1, Kind: Expand, Name: "0001_add.expand.sql", BinaryVersion: "1.0.0", MinCompatibleBinary: "1.0.0"},
	}
	applied := []Applied{
		{Version: 1, Kind: Expand, BinaryVersion: "1.0.0", MinCompatibleBinary: "1.0.0"},
		{Version: 2, Kind: Contract, BinaryVersion: "3.0.0", MinCompatibleBinary: "2.0.0"},
	}
	if err := CheckCompatibility("2.0.0", applied, oldCatalog); !errors.Is(err, ErrSchemaNewer) || !IsIncompatible(err) {
		t.Fatalf("unknown contract error = %v, want ErrSchemaNewer", err)
	}
	if err := CheckCompatibility("1.9.9", applied, oldCatalog); !errors.Is(err, ErrBinaryTooOld) || !IsIncompatible(err) {
		t.Fatalf("last reader error = %v, want ErrBinaryTooOld", err)
	}
}

func TestDevelopmentVersionIsNewestOnlyInTests(t *testing.T) {
	applied := []Applied{{
		Version: 1, Kind: Expand, BinaryVersion: "2.0.0", MinCompatibleBinary: "2.0.0",
	}}
	if err := CheckCompatibility("dev", applied, nil); err == nil {
		t.Fatal("production compatibility accepted an unstamped development binary")
	}
	if err := checkCompatibility("dev", applied, nil, true); err != nil {
		t.Fatalf("test development version was not ordered newest: %v", err)
	}
}

func TestMigrateRefusesWithoutBackupRef(t *testing.T) {
	err := (Runner{}).Apply(t.Context())
	if !errors.Is(err, ErrBackupRequired) {
		t.Fatalf("Apply error = %v, want ErrBackupRequired", err)
	}
}

func TestMigrationsTagged(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) == 0 {
		t.Fatal("embedded migration catalog is empty")
	}
	if err := ValidateCatalog(append(catalog, Migration{Version: 2, Kind: "untagged"})); err == nil {
		t.Fatal("untagged migration accepted")
	}
}

func TestReadmodelRebuildFlip(t *testing.T) {
	builder := &recordingReadmodelBuilder{}
	if err := RebuildReadmodel(t.Context(), builder, "run_summary", 3); err != nil {
		t.Fatal(err)
	}
	want := []string{"build:run_summary_v3", "flip:run_summary:3"}
	if !reflect.DeepEqual(builder.calls, want) {
		t.Fatalf("calls = %v, want %v", builder.calls, want)
	}

	builder = &recordingReadmodelBuilder{buildErr: errors.New("rebuild failed")}
	if err := RebuildReadmodel(t.Context(), builder, "run_summary", 4); err == nil {
		t.Fatal("failed rebuild reported success")
	}
	if len(builder.calls) != 1 {
		t.Fatalf("pointer flipped after failed rebuild: %v", builder.calls)
	}
}

func TestPartitionIndexesAreConcurrentThenAttached(t *testing.T) {
	executor := &recordingExecutor{}
	err := CreatePartitionedIndex(t.Context(), executor, "event_created_idx", []string{"created_at"}, []PartitionIndex{
		{Table: "event_2026_10", Index: "event_2026_10_created_idx"},
		{Table: "event_2026_11", Index: "event_2026_11_created_idx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS "event_2026_10_created_idx" ON "event_2026_10" ("created_at")`,
		`ALTER INDEX "event_created_idx" ATTACH PARTITION "event_2026_10_created_idx"`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS "event_2026_11_created_idx" ON "event_2026_11" ("created_at")`,
		`ALTER INDEX "event_created_idx" ATTACH PARTITION "event_2026_11_created_idx"`,
	}
	if !reflect.DeepEqual(executor.statements, want) {
		t.Fatalf("statements = %#v, want %#v", executor.statements, want)
	}
}

type recordingReadmodelBuilder struct {
	calls    []string
	buildErr error
}

type recordingExecutor struct {
	statements []string
}

func (e *recordingExecutor) Exec(_ context.Context, statement string, _ ...any) (pgconn.CommandTag, error) {
	e.statements = append(e.statements, statement)
	return pgconn.NewCommandTag("CREATE INDEX"), nil
}

func (e *recordingExecutor) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	if strings.Contains(query, "FROM pg_index") {
		return fakeRow{err: pgx.ErrNoRows}
	}
	return fakeRow{}
}

type fakeRow struct {
	err error
}

func (r fakeRow) Scan(destinations ...any) error {
	if r.err != nil {
		return r.err
	}
	*destinations[0].(*bool) = false
	return nil
}

func (b *recordingReadmodelBuilder) Build(_ context.Context, table string) error {
	b.calls = append(b.calls, "build:"+table)
	return b.buildErr
}

func (b *recordingReadmodelBuilder) Flip(_ context.Context, name string, version int) error {
	b.calls = append(b.calls, "flip:"+name+":"+strconv.Itoa(version))
	return nil
}
