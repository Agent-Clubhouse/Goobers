package cutover

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	cloudmigrate "github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgroles"
)

// Issues that gate the unfinished backends. Update the constant and delete the
// UnavailableError once the backend lands.
const (
	issueObjectStore    = 7813 // HA-P3-13: object-store blob store
	issueAdmissionPause = 7818 // HA-P3-18: control role and admission pause
	issueBanditImport   = 7763 // engine task.experiment: banditstore.ImportLocalJournals
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

// Connect supplies the two database identities bootstrap uses. Both are
// opened lazily so a precheck probes them without any earlier step having run:
// Admin (server administrator; creates roles and databases) and Owner (the
// instance database owner; the only identity that runs migrations). The
// owner cannot be probed before provisioning creates its role, so only the
// admin connection is probed up front.
type Connect struct {
	Admin func(ctx context.Context) (*pgx.Conn, error)
	Owner func(ctx context.Context) (*pgxpool.Pool, error)
	// Report receives non-fatal migration findings; nil drops them.
	Report func(error)
}

// PGSteps returns the ordered bootstrap steps for a PostgreSQL install. The
// returned func releases the owner pool the steps opened.
func PGSteps(connect Connect, instanceID, leaseName, backupRef, holder string, entra map[string]string) ([]Step, func()) {
	var owner *pgxpool.Pool
	open := func(ctx context.Context) (*pgxpool.Pool, error) {
		if owner != nil {
			return owner, nil
		}
		pool, err := connect.Owner(ctx)
		if err != nil {
			return nil, err
		}
		owner = pool
		return owner, nil
	}
	closeOwner := func() {
		if owner != nil {
			owner.Close()
		}
	}
	ownerConfigured := func(context.Context) error {
		if connect.Owner == nil {
			return errors.New("no owner database connection configured")
		}
		return nil
	}
	steps := []Step{
		{Name: "create database and roles", Check: func(ctx context.Context) error {
			if connect.Admin == nil {
				return errors.New("no administrator database connection configured")
			}
			conn, err := connect.Admin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close(ctx) }()
			return conn.Ping(ctx)
		}, Run: func(ctx context.Context) (bool, error) {
			conn, err := connect.Admin(ctx)
			if err != nil {
				return false, err
			}
			defer func() { _ = conn.Close(ctx) }()
			return pgroles.Provision(ctx, conn, pgroles.Plan{Instance: instanceID, Entra: entra})
		}},
		{Name: "apply schema migrations", Check: ownerConfigured, Run: func(ctx context.Context) (bool, error) {
			direct, err := open(ctx)
			if err != nil {
				return false, err
			}
			before, err := appliedOrNone(ctx, direct)
			if err != nil {
				return false, err
			}
			catalog, err := cloudmigrate.Catalog()
			if err != nil {
				return false, err
			}
			if err := (cloudmigrate.Runner{Direct: direct, Catalog: catalog, BackupRef: backupRef, InstanceID: instanceID, Holder: holder, Report: connect.Report}).Apply(ctx); err != nil {
				return false, err
			}
			after, err := cloudmigrate.AppliedMigrations(ctx, direct)
			return len(after) != len(before), err
		}},
		{Name: "create first lease row", Check: ownerConfigured, Run: func(ctx context.Context) (bool, error) {
			direct, err := open(ctx)
			if err != nil {
				return false, err
			}
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
	return steps, closeOwner
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
