package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
)

const (
	migrateLeaseLock   int64 = 0x676f6f6265727301
	migrateSessionLock int64 = 0x676f6f6265727302
	leaseSchemaVersion       = 2
)

var (
	// ErrBackupRequired means no restorable backup or PITR mark was supplied.
	ErrBackupRequired = errors.New("cloud migration requires a backup or PITR reference")
	// ErrMigrationBusy means another migration owner holds a required lock.
	ErrMigrationBusy = errors.New("cloud migration lock is already held")
)

// Runner applies an embedded catalog through the direct PostgreSQL pool.
type Runner struct {
	Direct           *pgxpool.Pool
	Catalog          []Migration
	BackupRef        string
	InstanceID       string
	Holder           string
	PartitionIndexes []PartitionIndexPlan
	Readmodels       []ReadmodelPlan
	ReadmodelBuilder ReadmodelBuilder
}

// PartitionIndexPlan describes one parent index and all child indexes.
type PartitionIndexPlan struct {
	Parent     string
	Columns    []string
	Partitions []PartitionIndex
}

// ReadmodelPlan describes a versioned read model rebuild.
type ReadmodelPlan struct {
	Name    string
	Version int
}

// Apply takes a migrate lease and a separate direct-session advisory lock,
// then applies each pending migration exactly once.
func (r Runner) Apply(ctx context.Context) error {
	if strings.TrimSpace(r.BackupRef) == "" {
		return ErrBackupRequired
	}
	if strings.TrimSpace(r.InstanceID) == "" || strings.TrimSpace(r.Holder) == "" {
		return errors.New("cloud migration instance and holder are required")
	}
	if r.Direct == nil {
		return errors.New("cloud migration direct pool is required")
	}
	if err := ValidateCatalog(r.Catalog); err != nil {
		return err
	}
	lease, err := acquireLock(ctx, r.Direct, migrateLeaseLock)
	if err != nil {
		return fmt.Errorf("cloud migration lease: %w", err)
	}
	defer lease.release()

	session, err := acquireLock(ctx, r.Direct, migrateSessionLock)
	if err != nil {
		return fmt.Errorf("cloud migration session lock: %w", err)
	}
	defer session.release()

	applied, err := loadApplied(ctx, session.conn)
	if err != nil {
		return err
	}
	for _, migration := range r.Catalog {
		if migration.Version > leaseSchemaVersion {
			continue
		}
		if applied[migration.Version] {
			continue
		}
		if err := applyOne(ctx, session.conn, migration, r.BackupRef); err != nil {
			return err
		}
	}
	lease.release()

	manager, err := cloudlease.New(cloudlease.Config{
		Pool: r.Direct, InstanceID: r.InstanceID, Name: "migrate", Holder: r.Holder,
	})
	if err != nil {
		return err
	}
	return manager.Run(ctx, func(runCtx context.Context, handle *cloudlease.Handle) error {
		for _, migration := range r.Catalog {
			if migration.Version <= leaseSchemaVersion || applied[migration.Version] {
				continue
			}
			err := manager.WithFence(runCtx, handle, func(txCtx context.Context, tx pgx.Tx) error {
				return applyInTx(txCtx, tx, migration, r.BackupRef)
			})
			if err != nil {
				return err
			}
		}
		for _, index := range r.PartitionIndexes {
			if err := CreatePartitionedIndex(runCtx, session.conn, index.Parent, index.Columns, index.Partitions); err != nil {
				return err
			}
		}
		if len(r.Readmodels) > 0 && r.ReadmodelBuilder == nil {
			return errors.New("cloud migration read-model builder is required")
		}
		for _, readmodel := range r.Readmodels {
			if err := RebuildReadmodel(runCtx, r.ReadmodelBuilder, readmodel.Name, readmodel.Version); err != nil {
				return err
			}
		}
		return nil
	})
}

type heldLock struct {
	conn *pgxpool.Conn
	key  int64
}

func acquireLock(ctx context.Context, pool *pgxpool.Pool, key int64) (*heldLock, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, err
	}
	if !acquired {
		conn.Release()
		return nil, ErrMigrationBusy
	}
	return &heldLock{conn: conn, key: key}, nil
}

func (lock *heldLock) release() {
	if lock.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = lock.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lock.key)
	lock.conn.Release()
	lock.conn = nil
}

func loadApplied(ctx context.Context, conn *pgxpool.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM control.schema_migration ORDER BY version")
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "42P01" {
			return map[int]bool{}, nil
		}
		return nil, fmt.Errorf("load cloud migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("load cloud migration version: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load cloud migrations: %w", err)
	}
	return applied, nil
}

func applyOne(ctx context.Context, conn *pgxpool.Conn, migration Migration, backupRef string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cloud migration %d: %w", migration.Version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := applyInTx(ctx, tx, migration, backupRef); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit cloud migration %d: %w", migration.Version, err)
	}
	return nil
}

func applyInTx(ctx context.Context, tx pgx.Tx, migration Migration, backupRef string) error {
	if _, err := tx.Exec(ctx, migration.SQL); err != nil {
		return fmt.Errorf("apply cloud migration %d: %w", migration.Version, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.schema_migration
			(version, kind, binary_version, min_compatible_binary, backup_ref, applied_at)
		VALUES ($1, $2, $3, $4, $5, now())`,
		migration.Version, migration.Kind, migration.BinaryVersion, migration.MinCompatibleBinary, backupRef,
	); err != nil {
		return fmt.Errorf("record cloud migration %d: %w", migration.Version, err)
	}
	return nil
}

// AppliedMigrations loads compatibility metadata for startup readiness checks.
func AppliedMigrations(ctx context.Context, pool *pgxpool.Pool) ([]Applied, error) {
	rows, err := pool.Query(ctx, `
		SELECT version, kind, binary_version, min_compatible_binary
		FROM control.schema_migration ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Applied])
	if err != nil {
		return nil, fmt.Errorf("load cloud schema compatibility: %w", err)
	}
	return applied, nil
}

// CheckPoolCompatibility performs the startup/readiness schema check.
func CheckPoolCompatibility(ctx context.Context, pool *pgxpool.Pool, binary string) error {
	catalog, err := Catalog()
	if err != nil {
		return err
	}
	applied, err := AppliedMigrations(ctx, pool)
	if err != nil {
		return fmt.Errorf("load cloud schema for compatibility check: %w", err)
	}
	return CheckCompatibility(binary, applied, catalog)
}
