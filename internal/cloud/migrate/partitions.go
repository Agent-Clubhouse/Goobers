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

// partitionMaintenanceLock serializes partition maintenance across migrate Jobs
// and the control role's periodic job.
const partitionMaintenanceLock int64 = 0x676f6f6265727303

// partitionLockTries bounds the DEFAULT lock retries (tests lower it).
var partitionLockTries = 20

const (
	partitionMoveBatch    = 5000
	partitionBatchTimeout = time.Minute
)

// PartitionPlan describes one monthly range-partitioned table that keeps its
// DEFAULT partition empty by pre-creating months. The table must have the
// primary key (instance_id, stream, seq, <Column>) the cloud journal uses.
type PartitionPlan struct {
	// Schema and Table name the partitioned parent, e.g. journal.instance_event.
	Schema, Table string
	// Column is the timestamptz range key.
	Column string
	// UniqueIndex and UniqueWhere, when set, describe a partial unique index
	// built on each new partition BEFORE it is attached. The table is private
	// until then, so a plain CREATE UNIQUE INDEX needs no CONCURRENTLY.
	UniqueIndex, UniqueWhere string
	// MonthsAhead is how many months after the current one to pre-create.
	MonthsAhead int
}

// DefaultPartitionPlans are the monthly tables the cloud journal owns.
var DefaultPartitionPlans = []PartitionPlan{
	{Schema: "journal", Table: "instance_event", Column: "at", MonthsAhead: 3},
	{Schema: "journal", Table: "health_event", Column: "at",
		UniqueIndex: "instance_id, stream, emit_key", UniqueWhere: "emit_key IS NOT NULL", MonthsAhead: 3},
}

// PartitionMaintainer creates and fills monthly partitions under a lease.
// Handle returns the caller's current lease (migrate or controller); a nil
// handle skips the pass.
type PartitionMaintainer struct {
	Pool   *pgxpool.Pool
	Lease  *cloudlease.Manager
	Handle func() *cloudlease.Handle
	Plans  []PartitionPlan
	// DeferCurrentMonth leaves the current month's rows in DEFAULT. The migrate
	// Job sets it: the previous binary is still serving then, has no retry for
	// the one-instant ATTACH routing race, and only the current month has live
	// writers. The controller's periodic pass (new binary only) attaches it.
	DeferCurrentMonth bool
}

// Ensure runs one maintenance pass. It is idempotent and resumable: a
// half-built partition left by a crash is a private, unattached table that the
// next pass keeps filling.
//
// The expensive part never runs inside a short lease fence. Rows already in the
// DEFAULT partition (a live install, or a month the horizon missed, including
// PAST months) are moved into the standalone table in bounded batches, each its
// own short transaction on a session holding the maintenance advisory lock,
// with lease ownership re-verified before every batch. Only the final step,
// which blocks writers, is fenced: it moves the stragglers written meanwhile
// and ATTACHes. The standalone table already carries the range CHECK and the
// foreign keys, so ATTACH validates nothing about it; the DEFAULT scan it needs
// is bounded because maintenance keeps DEFAULT near-empty.
func (m PartitionMaintainer) Ensure(ctx context.Context) error {
	if m.Pool == nil || m.Lease == nil || m.Handle == nil {
		return errors.New("cloud partition maintenance requires a pool, lease manager and handle")
	}
	lock, err := acquireLock(ctx, m.Pool, partitionMaintenanceLock)
	if err != nil {
		return err
	}
	defer lock.release()
	return m.ensureOn(ctx, lock.conn)
}

// EnsureOn runs a pass on a connection the caller already holds (the migrate
// runner's advisory-locked session), taking the maintenance lock on it too.
func (m PartitionMaintainer) EnsureOn(ctx context.Context, conn *pgxpool.Conn) error {
	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", partitionMaintenanceLock).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return ErrMigrationBusy
	}
	defer func() {
		unlock, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlock, "SELECT pg_advisory_unlock($1)", partitionMaintenanceLock)
	}()
	return m.ensureOn(ctx, conn)
}

func (m PartitionMaintainer) ensureOn(ctx context.Context, conn *pgxpool.Conn) error {
	handle := m.Handle()
	if handle == nil {
		return nil
	}
	for _, plan := range m.Plans {
		if err := plan.validate(); err != nil {
			return err
		}
		if err := m.ensurePlan(ctx, conn, handle, plan); err != nil {
			return fmt.Errorf("partition maintenance for %s.%s: %w", plan.Schema, plan.Table, err)
		}
	}
	return nil
}

// Run calls Ensure now and then every interval until ctx ends. ErrMigrationBusy
// (another maintainer holds the lock) is not an error; other failures go to
// report and the next tick retries.
func (m PartitionMaintainer) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval <= 0 {
		return errors.New("cloud partition maintenance interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := m.Ensure(ctx); err != nil && !errors.Is(err, ErrMigrationBusy) && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (p PartitionPlan) validate() error {
	if p.Schema == "" || p.Table == "" || p.Column == "" || p.MonthsAhead < 0 {
		return errors.New("cloud partition plan requires schema, table, column and a non-negative horizon")
	}
	return nil
}

func (p PartitionPlan) parent() string { return pgx.Identifier{p.Schema, p.Table}.Sanitize() }
func (p PartitionPlan) deflt() string {
	return pgx.Identifier{p.Schema, p.Table + "_default"}.Sanitize()
}
func (p PartitionPlan) column() string       { return pgx.Identifier{p.Column}.Sanitize() }
func (p PartitionPlan) name(s string) string { return p.Table + "_" + s }

type month struct {
	start, end time.Time
	suffix     string
}

func (m PartitionMaintainer) ensurePlan(ctx context.Context, conn *pgxpool.Conn, handle *cloudlease.Handle, plan PartitionPlan) error {
	var present bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, plan.parent()).Scan(&present); err != nil || !present {
		return err
	}
	months, err := plan.months(ctx, conn, m.DeferCurrentMonth)
	if err != nil {
		return err
	}
	for _, mo := range months {
		if err := m.ensureMonth(ctx, conn, handle, plan, mo); err != nil {
			return fmt.Errorf("%s: %w", mo.suffix, err)
		}
	}
	return nil
}

// months lists the current month through MonthsAhead (DB clock, UTC) plus every
// earlier month that still has rows in DEFAULT, so DEFAULT shrinks over time.
func (p PartitionPlan) months(ctx context.Context, conn *pgxpool.Conn, deferCurrent bool) ([]month, error) {
	firstOffset := 0
	if deferCurrent {
		firstOffset = 1
	}
	rows, err := conn.Query(ctx, fmt.Sprintf(`
		WITH bounds AS (
			SELECT date_trunc('month', clock_timestamp() AT TIME ZONE 'UTC') AS this_month
		), wanted AS (
			SELECT this_month + make_interval(months => n) AS s
			FROM bounds, generate_series($2::int, $1::int) AS n
			UNION
			SELECT DISTINCT date_trunc('month', %[1]s AT TIME ZONE 'UTC')
			FROM %[2]s, bounds
			WHERE %[1]s < (bounds.this_month AT TIME ZONE 'UTC')
		)
		SELECT s AT TIME ZONE 'UTC', (s + interval '1 month') AT TIME ZONE 'UTC', to_char(s, 'YYYYMM')
		FROM wanted ORDER BY s`, p.column(), p.deflt()), p.MonthsAhead, firstOffset)
	if err != nil {
		return nil, fmt.Errorf("list partition months: %w", err)
	}
	defer rows.Close()
	var out []month
	for rows.Next() {
		var mo month
		if err := rows.Scan(&mo.start, &mo.end, &mo.suffix); err != nil {
			return nil, err
		}
		out = append(out, mo)
	}
	return out, rows.Err()
}

func (m PartitionMaintainer) ensureMonth(ctx context.Context, conn *pgxpool.Conn, handle *cloudlease.Handle, plan PartitionPlan, mo month) error {
	name := plan.name(mo.suffix)
	child := pgx.Identifier{plan.Schema, name}.Sanitize()
	var attached, exists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_inherits WHERE inhparent=to_regclass($1) AND inhrelid=to_regclass($2)),
		       to_regclass($2) IS NOT NULL`, plan.parent(), child).Scan(&attached, &exists); err != nil {
		return fmt.Errorf("inspect partition %s: %w", name, err)
	}
	if attached {
		return nil
	}
	return m.moveAndAttach(ctx, conn, handle, plan, name, child, mo, exists)
}

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "55P03"
	}
	return err != nil && strings.Contains(err.Error(), "55P03")
}

func (m PartitionMaintainer) verify(ctx context.Context, handle *cloudlease.Handle) error {
	if err := m.Lease.WithFence(ctx, handle, func(context.Context, pgx.Tx) error { return nil }); err != nil {
		return fmt.Errorf("verify lease before partition step: %w", err)
	}
	return nil
}

// prepare builds the private, unattached table inside the caller's transaction,
// so a failed pass leaves no table behind: a leftover unattached table in the
// journal schema would have no write-path inventory entry and fail every later
// grant reconcile.
func (p PartitionPlan) prepare(ctx context.Context, tx pgx.Tx, name, child string, mo month) error {
	statements := []string{
		fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES)", child, p.parent()),
		fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s >= %s AND %s < %s)",
			child, pgx.Identifier{name + "_range"}.Sanitize(), p.column(), literal(mo.start), p.column(), literal(mo.end)),
	}
	if p.UniqueIndex != "" {
		statements = append(statements, fmt.Sprintf("CREATE UNIQUE INDEX %s ON %s (%s) WHERE %s",
			pgx.Identifier{name + "_emit_key"}.Sanitize(), child, p.UniqueIndex, p.UniqueWhere))
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("prepare partition %s: %w", name, err)
		}
	}
	// Pre-create the parent's foreign keys on the empty table so ATTACH reuses
	// them instead of scanning the filled partition under its lock.
	fks, err := tx.Query(ctx, `
		SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid=to_regclass($1) AND contype='f'`, p.parent())
	if err != nil {
		return err
	}
	type fk struct{ name, def string }
	var defs []fk
	for fks.Next() {
		var f fk
		if err := fks.Scan(&f.name, &f.def); err != nil {
			fks.Close()
			return err
		}
		defs = append(defs, f)
	}
	fks.Close()
	if err := fks.Err(); err != nil {
		return err
	}
	for _, f := range defs {
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s",
			child, pgx.Identifier{f.name}.Sanitize(), f.def)); err != nil {
			return fmt.Errorf("prepare partition %s foreign key: %w", name, err)
		}
	}
	return nil
}

// moveStatement moves one keyset batch from DEFAULT into the private table and
// returns the largest key moved (no row means nothing left past the cursor).
func (p PartitionPlan) moveStatement(child string) string {
	return fmt.Sprintf(`
		WITH batch AS (
			SELECT instance_id, stream, seq, %[1]s FROM %[2]s
			WHERE %[1]s >= $1 AND %[1]s < $2 AND (instance_id, stream, seq) > ($3, $4, $5::bigint)
			ORDER BY instance_id, stream, seq LIMIT %[4]d
		), moved AS (
			DELETE FROM %[2]s d USING batch b
			WHERE d.instance_id=b.instance_id AND d.stream=b.stream AND d.seq=b.seq AND d.%[1]s=b.%[1]s
			RETURNING d.*
		), inserted AS (
			INSERT INTO %[3]s SELECT * FROM moved RETURNING instance_id, stream, seq
		)
		SELECT instance_id, stream, seq FROM inserted ORDER BY instance_id DESC, stream DESC, seq DESC LIMIT 1`,
		p.column(), p.deflt(), child, partitionMoveBatch)
}

type moveCursor struct {
	instanceID, stream string
	seq                int64
}

func (p PartitionPlan) moveBatch(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, child string, mo month, cur moveCursor) (moveCursor, bool, error) {
	var next moveCursor
	err := q.QueryRow(ctx, p.moveStatement(child), mo.start, mo.end, cur.instanceID, cur.stream, cur.seq).
		Scan(&next.instanceID, &next.stream, &next.seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return cur, false, nil
	}
	return next, err == nil, err
}

// moveAndAttach moves DEFAULT's rows for the month into the private table and
// attaches it in ONE transaction on the maintenance session. Nothing is visible
// to readers in between: until COMMIT every row is still in DEFAULT, and a
// failure at any point (a lock that cannot be taken, a lost lease, a timeout)
// rolls everything back with every row still visible through the parent. That
// matters because readers follow durable (stream, seq) cursors; moving rows out
// of view in committed batches would let a cursor advance past events it never
// saw.
//
// The transaction is not lease-fenced as a whole (WithFence's share lock on the
// lease row would block lease renewal for its duration). Ownership is verified
// before it starts and confirmed, with a share lock, immediately before COMMIT.
// Row movement uses bounded statements and takes only row locks, so appends keep
// flowing; only the final sweep + ATTACH hold the DEFAULT table lock, with a
// lock_timeout and savepoint retries so a long writer cannot make it queue.
func (m PartitionMaintainer) moveAndAttach(ctx context.Context, conn *pgxpool.Conn, handle *cloudlease.Handle, plan PartitionPlan, name, child string, mo month, exists bool) error {
	if err := m.verify(ctx, handle); err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf("SELECT set_config('statement_timeout', '%dms', true)", partitionBatchTimeout.Milliseconds())); err != nil {
		return err
	}
	if !exists {
		if err := plan.prepare(ctx, tx, name, child, mo); err != nil {
			return err
		}
	}
	if err := plan.moveAll(ctx, tx, child, mo); err != nil {
		return err
	}
	if err := plan.lockDefault(ctx, tx); err != nil {
		return fmt.Errorf("attach partition %s: %w", name, err)
	}
	// Stragglers written since the bulk move; writers are blocked now.
	if err := plan.moveAll(ctx, tx, child, mo); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ATTACH PARTITION %s FOR VALUES FROM (%s) TO (%s)",
		plan.parent(), child, literal(mo.start), literal(mo.end))); err != nil {
		return fmt.Errorf("attach partition %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", child, pgx.Identifier{name + "_range"}.Sanitize())); err != nil {
		return fmt.Errorf("drop partition range check %s: %w", name, err)
	}
	if err := m.confirm(ctx, tx, handle); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit partition %s: %w", name, err)
	}
	return nil
}

// moveAll runs keyset batches until DEFAULT holds no more rows for the month.
func (p PartitionPlan) moveAll(ctx context.Context, tx pgx.Tx, child string, mo month) error {
	var cur moveCursor
	for {
		next, more, err := p.moveBatch(ctx, tx, child, mo, cur)
		if err != nil {
			return fmt.Errorf("move rows into %s: %w", child, err)
		}
		if !more {
			return nil
		}
		cur = next
	}
}

// lockDefault takes the DEFAULT table lock that ATTACH needs, retrying on a
// savepoint so one slow writer neither aborts the whole move nor makes the
// request queue (and with it every later append) behind itself.
func (p PartitionPlan) lockDefault(ctx context.Context, tx pgx.Tx) error {
	var last error
	for attempt := 0; attempt < partitionLockTries; attempt++ {
		if _, err := tx.Exec(ctx, "SAVEPOINT partition_lock"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '500ms'"); err != nil {
			return err
		}
		_, last = tx.Exec(ctx, fmt.Sprintf("LOCK TABLE %s IN SHARE ROW EXCLUSIVE MODE", p.deflt()))
		if last == nil {
			return nil
		}
		if !isLockTimeout(last) {
			return last
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT partition_lock"); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return last
}

// confirm re-checks lease ownership inside the move transaction and share-locks
// the lease row until COMMIT, so a takeover cannot commit in between.
func (m PartitionMaintainer) confirm(ctx context.Context, tx pgx.Tx, handle *cloudlease.Handle) error {
	if m.Lease.SelfFenced(handle) {
		return cloudlease.ErrSelfFenced
	}
	var one int
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM control.lease
		WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4 FOR SHARE`,
		handle.InstanceID, handle.Name, handle.Holder, handle.Epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return cloudlease.ErrStale
	}
	return err
}

func literal(t time.Time) string {
	return "'" + t.UTC().Format("2006-01-02 15:04:05.999999-07") + "'::timestamptz"
}

// DropInstanceEventPartition is the retention operation for scheduler history.
// instance_emit_key is unpartitioned, so dropping a month without deleting the
// keys it holds would leak dedup rows forever. Both happen in the caller's
// transaction; retention code must use this and not DROP the partition itself.
func DropInstanceEventPartition(ctx context.Context, tx pgx.Tx, suffix string) error {
	if len(suffix) != 6 || strings.Trim(suffix, "0123456789") != "" {
		return fmt.Errorf("partition suffix %q must be YYYYMM", suffix)
	}
	child := pgx.Identifier{"journal", "instance_event_" + suffix}.Sanitize()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		DELETE FROM journal.instance_emit_key k
		WHERE EXISTS (SELECT 1 FROM %s e
		              WHERE e.instance_id=k.instance_id AND e.stream=k.stream AND e.emit_key=k.emit_key)`, child)); err != nil {
		return fmt.Errorf("delete emit keys for %s: %w", suffix, err)
	}
	if _, err := tx.Exec(ctx, "ALTER TABLE journal.instance_event DETACH PARTITION "+child); err != nil {
		return fmt.Errorf("detach %s: %w", suffix, err)
	}
	if _, err := tx.Exec(ctx, "DROP TABLE "+child); err != nil {
		return fmt.Errorf("drop %s: %w", suffix, err)
	}
	return nil
}
