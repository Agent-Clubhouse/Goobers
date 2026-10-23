package cutover

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	cloudmigrate "github.com/goobers/goobers/internal/cloud/migrate"
)

// Issues that gate the unfinished backends. Update the constant and delete the
// UnavailableError once the backend lands.
const (
	issueProvisionDatabase = 7821 // HA-P3-21: database and role provisioning references
	issueObjectStore       = 7813 // HA-P3-13: object-store blob store
	issueAdmissionPause    = 7818 // HA-P3-18: control role and admission pause
	issueBanditImport      = 7763 // engine task.experiment: banditstore.ImportLocalJournals
)

// ControllerLeaseName is the lease row the control role acquires. It mirrors
// lease.ControllerName (#7859); switch to that constant when it lands.
const ControllerLeaseName = "controller"

// PGInstall observes a PostgreSQL-backed install.
type PGInstall struct {
	Pool       *pgxpool.Pool
	Binary     string
	InstanceID string
	LeaseName  string
}

// SchemaCompatible reports whether this binary may use the installed schema.
func (p PGInstall) SchemaCompatible(ctx context.Context) error {
	return cloudmigrate.CheckPoolCompatibility(ctx, p.Pool, p.Binary)
}

// LeaseRowPresent reports whether bootstrap created the first lease row.
func (p PGInstall) LeaseRowPresent(ctx context.Context) (bool, error) {
	var present bool
	err := p.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM control.lease WHERE instance_id = $1 AND name = $2)`,
		p.InstanceID, p.LeaseName).Scan(&present)
	return present, err
}

// AdmissionsPaused is unavailable until the admission pause exists.
func (PGInstall) AdmissionsPaused(context.Context) (bool, error) {
	return false, &UnavailableError{What: "admission pause state", Issue: issueAdmissionPause}
}

// BucketReachable is unavailable until the object store exists.
func (PGInstall) BucketReachable(context.Context) error {
	return &UnavailableError{What: "object-store reachability", Issue: issueObjectStore}
}

// PGSteps returns the ordered bootstrap steps for a PostgreSQL install.
func PGSteps(direct *pgxpool.Pool, instanceID, leaseName, backupRef, holder string) []Step {
	ping := func(ctx context.Context) error { return direct.Ping(ctx) }
	return []Step{
		{Name: "create database and roles", Issue: issueProvisionDatabase},
		{Name: "apply schema migrations", Check: ping, Run: func(ctx context.Context) (bool, error) {
			before, err := appliedOrNone(ctx, direct)
			if err != nil {
				return false, err
			}
			catalog, err := cloudmigrate.Catalog()
			if err != nil {
				return false, err
			}
			if err := (cloudmigrate.Runner{Direct: direct, Catalog: catalog, BackupRef: backupRef, InstanceID: instanceID, Holder: holder}).Apply(ctx); err != nil {
				return false, err
			}
			after, err := cloudmigrate.AppliedMigrations(ctx, direct)
			return len(after) != len(before), err
		}},
		{Name: "create first lease row", Check: ping, Run: func(ctx context.Context) (bool, error) {
			tx, err := direct.Begin(ctx)
			if err != nil {
				return false, err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var existed bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM control.lease WHERE instance_id = $1 AND name = $2)`,
				instanceID, leaseName).Scan(&existed); err != nil {
				return false, err
			}
			if err := lease.EnsureRows(ctx, tx, instanceID, leaseName); err != nil {
				return false, err
			}
			if err := tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("commit lease row: %w", err)
			}
			return !existed, nil
		}},
		{Name: "create object-store bucket", Issue: issueObjectStore},
		{Name: "pause admissions", Issue: issueAdmissionPause},
		// One-time, idempotent import of local bandit observations. Becomes a
		// Check (store reachable, runs dir readable) plus a Run that reports
		// ImportResult{Imported, Existing} once banditstore lands.
		{Name: "import local bandit observations", Issue: issueBanditImport},
	}
}

// appliedOrNone reads the migration ledger, treating only a missing ledger
// table (first run) as "nothing applied"; any other error is real.
func appliedOrNone(ctx context.Context, pool *pgxpool.Pool) ([]cloudmigrate.Applied, error) {
	applied, err := cloudmigrate.AppliedMigrations(ctx, pool)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return nil, nil
	}
	return applied, err
}
