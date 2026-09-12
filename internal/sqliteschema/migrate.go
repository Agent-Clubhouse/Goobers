// Package sqliteschema applies versioned migrations to embedded SQLite stores.
package sqliteschema

import (
	"context"
	"database/sql"
	"fmt"
)

// Migrate applies pending migrations and advances schema_meta atomically. The
// database must use SQLite's immediate transaction lock so concurrent openers
// cannot both observe and migrate the same version.
func Migrate(ctx context.Context, db *sql.DB, store string, migrations []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin schema migration: %w", store, err)
	}
	defer func() { _ = tx.Rollback() }()

	var metadataExisted int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'schema_meta'`).Scan(&metadataExisted); err != nil {
		return fmt.Errorf("%s: inspect schema metadata: %w", store, err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("%s: create schema metadata: %w", store, err)
	}
	var count, version int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(version), 0) FROM schema_meta`).Scan(&count, &version); err != nil {
		return fmt.Errorf("%s: read schema version: %w", store, err)
	}
	if (metadataExisted == 1 && count != 1) || count > 1 {
		return fmt.Errorf("%s: schema_meta contains %d rows; restore the database from backup", store, count)
	}
	if version < 0 {
		return fmt.Errorf("%s: store schema version %d is invalid; restore the database from backup", store, version)
	}
	if version > len(migrations) {
		return fmt.Errorf(
			"%s: store schema version %d is newer than this build supports (%d); upgrade this binary or restore a compatible database",
			store, version, len(migrations),
		)
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("%s: apply schema migration %d: %w", store, i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_meta`); err != nil {
			return fmt.Errorf("%s: reset schema metadata after migration %d: %w", store, i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_meta (version) VALUES (?)`, i+1); err != nil {
			return fmt.Errorf("%s: record schema version %d: %w", store, i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit schema migration: %w", store, err)
	}
	return nil
}
