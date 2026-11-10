package blockedrecordspg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/blockedrecords"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
)

// A cycle that cannot be verified yet is not escalated on a guess and not parked
// forever: its seed record carries a deferred marker (database clock) that survives
// a failover, and Sweep re-checks it. The sweep drops the cycle if it is false,
// escalates it if it is real, and escalates it as unverifiable once it has waited
// DefaultDeferralBound or fails permanently, so a deferred cycle always ends.
const (
	// DefaultDeferralBound is the longest a cycle may stay deferred.
	DefaultDeferralBound = time.Hour
	// MinDeferralBound clamps a configured bound: shorter would escalate before a
	// transient provider outage can clear.
	MinDeferralBound = 10 * time.Minute
	// FirstDeferralDelay is the wait before the first re-check; later waits double
	// up to MaxDeferralDelay.
	FirstDeferralDelay = 5 * time.Minute
	MaxDeferralDelay   = 30 * time.Minute

	// DefaultSweepCycles and DefaultSweepMembers bound one sweep pass.
	DefaultSweepCycles  = 5
	DefaultSweepMembers = 10
)

// ClampDeferralBound returns d, or the default when unset, never below the minimum.
func ClampDeferralBound(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultDeferralBound
	case d < MinDeferralBound:
		return MinDeferralBound
	}
	return d
}

// CycleKey names a cycle by its members, so the same cycle always yields the same
// key however it was found: escalation comments are keyed by it, which makes a
// repeated escalation (a crash between the writes and the marker clear) a no-op.
func CycleKey(members []blockedcycle.Node) string {
	keys := make([]string, 0, len(members))
	for _, node := range members {
		keys = append(keys, blockedrecords.Key(node.Repository, node.ItemID))
	}
	slices.Sort(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return "cycle-" + hex.EncodeToString(sum[:8])
}

// Deferral is a cycle awaiting re-verification.
type Deferral struct {
	// Key is the seed record's key.
	Key    string
	Record blockedrecords.Record
	// Age is how long it has been deferred, on the database clock.
	Age      time.Duration
	Attempts int
	Reason   string
}

// deferCycle marks the seed record's cycle deferred, due again after delay.
func (s Store) deferCycle(ctx context.Context, handle *lease.Handle, key, reason string, delay time.Duration) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE control.blocked_record
			SET verify_after = clock_timestamp() + $3::interval,
			    deferred_since = COALESCE(deferred_since, clock_timestamp()),
			    defer_attempts = defer_attempts + 1, defer_reason = $4, owner_epoch = $5
			WHERE instance_id=$1 AND record_key=$2`,
			handle.InstanceID, key, delay, reason, handle.Epoch)
		if err != nil {
			return fmt.Errorf("defer blocked-record cycle: %w", err)
		}
		return requireWritten(tag.RowsAffected())
	})
}

// clearDeferral drops the marker on key (a no-op without one).
func (s Store) clearDeferral(ctx context.Context, handle *lease.Handle, key string) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE control.blocked_record
			SET verify_after = NULL, deferred_since = NULL, defer_attempts = 0, defer_reason = '', owner_epoch = $3
			WHERE instance_id=$1 AND record_key=$2 AND verify_after IS NOT NULL`,
			handle.InstanceID, key, handle.Epoch)
		return err
	})
}

// due lists the deferrals whose re-check has fallen due, oldest first.
func (s Store) due(ctx context.Context, handle *lease.Handle, limit int) ([]Deferral, error) {
	var out []Deferral
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT record_key, record, defer_attempts, defer_reason,
			       extract(epoch FROM clock_timestamp() - deferred_since)
			FROM control.blocked_record
			WHERE instance_id=$1 AND verify_after IS NOT NULL AND verify_after <= clock_timestamp()
			ORDER BY verify_after LIMIT $2`, handle.InstanceID, limit)
		if err != nil {
			return fmt.Errorf("list due deferrals: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d Deferral
			var raw []byte
			var age float64
			if err := rows.Scan(&d.Key, &raw, &d.Attempts, &d.Reason, &age); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &d.Record); err != nil {
				return fmt.Errorf("decode blocked record %q: %w", d.Key, err)
			}
			d.Age = time.Duration(age * float64(time.Second))
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// Deferrals counts the markers outstanding (for readiness and tests).
func (s Store) Deferrals(ctx context.Context, handle *lease.Handle) (int, error) {
	var n int
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM control.blocked_record WHERE instance_id=$1 AND verify_after IS NOT NULL`, handle.InstanceID).Scan(&n)
	})
	return n, err
}

// refusedPrefix marks a seed record whose escalation the provider permanently
// refused. The row keeps verify_after and deferred_since NULL (so it is never due
// and the pair CHECK holds) and is told apart by defer_reason alone; it therefore
// survives a failover, and clears itself the way every marker does: a re-recorded
// block resets defer_reason, and Forget deletes the row.
const refusedPrefix = "refused: "

// refusedClass is the short, scrubbed class stored with a refusal: never the
// provider's own text.
func refusedClass(err error) string {
	if errors.Is(err, providerwriter.ErrRepositoryNotPinned) {
		return "repository not configured"
	}
	return "provider refused"
}

// markRefused latches the seed's cycle as permanently refused, ending its deferral.
func (s Store) markRefused(ctx context.Context, handle *lease.Handle, key, class string) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE control.blocked_record
			SET verify_after = NULL, deferred_since = NULL, defer_reason = $3, owner_epoch = $4
			WHERE instance_id=$1 AND record_key=$2`,
			handle.InstanceID, key, refusedPrefix+class, handle.Epoch)
		if err != nil {
			return fmt.Errorf("latch refused blocked-record cycle: %w", err)
		}
		return requireWritten(tag.RowsAffected())
	})
}

// MaxRefusedListed bounds the refused cycles one read returns.
const MaxRefusedListed = 50

// Refused lists the seeds whose cycle escalation stands permanently refused, from
// the database: a standing condition, not a process-local memory.
func (s Store) Refused(ctx context.Context, handle *lease.Handle) ([]string, error) {
	var keys []string
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT record_key FROM control.blocked_record
			WHERE instance_id=$1 AND starts_with(defer_reason, $2) ORDER BY record_key LIMIT $3`,
			handle.InstanceID, refusedPrefix, MaxRefusedListed)
		if err != nil {
			return fmt.Errorf("list refused cycles: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			keys = append(keys, key)
		}
		return rows.Err()
	})
	return keys, err
}

// releaseRefused drops the refusal latch on every member of a cycle that is being
// escalated afresh (through another seed, or by the sweep): the latch described an
// earlier attempt that this one supersedes. A failed attempt latches again.
func (s Store) releaseRefused(ctx context.Context, handle *lease.Handle, members []blockedcycle.Node) error {
	keys := make([]string, 0, len(members))
	for _, node := range members {
		keys = append(keys, blockedrecords.Key(node.Repository, node.ItemID))
	}
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE control.blocked_record SET defer_reason = '', owner_epoch = $4
			WHERE instance_id=$1 AND record_key = ANY($2) AND starts_with(defer_reason, $3)`,
			handle.InstanceID, keys, refusedPrefix, handle.Epoch)
		return err
	})
}

// reconcileRefused releases the refusals whose cycle no longer exists (a member
// closed or the dependency was removed without the seed being re-recorded). The
// refusals whose cycle stands are returned.
func (s Store) reconcileRefused(ctx context.Context, handle *lease.Handle) ([]string, error) {
	keys, err := s.Refused(ctx, handle)
	if err != nil || len(keys) == 0 {
		return nil, err
	}
	recs, err := s.snapshot(ctx, handle)
	if err != nil {
		return nil, err
	}
	var standing []string
	for _, key := range keys {
		if len(blockedrecords.FindCycle(recs, key).Affected) > 0 {
			standing = append(standing, key)
			continue
		}
		if err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE control.blocked_record SET defer_reason = '', owner_epoch = $3
				WHERE instance_id=$1 AND record_key=$2 AND starts_with(defer_reason, $4)`,
				handle.InstanceID, key, handle.Epoch, refusedPrefix)
			return err
		}); err != nil {
			return nil, err
		}
	}
	return standing, nil
}

func requireWritten(rows int64) error {
	if rows == 0 {
		return errors.New("blocked record is gone")
	}
	return nil
}

// ErrEscalationRefused means the provider permanently refused to escalate a
// deferred cycle (a refusal that retrying cannot change). The sweep stops retrying
// that cycle and reports it, so it is surfaced loudly instead of retried in
// silence forever.
var ErrEscalationRefused = errors.New("blocked-record cycle escalation permanently refused")

// EscalateFunc performs the provider writes that escalate a cycle (needs-human on
// every member, the cycle report, a journal entry); cycle.Unverified names what
// could not be confirmed. A failure keeps the marker, so the next pass retries it.
type EscalateFunc func(ctx context.Context, cycle blockedcycle.Result, seed Deferral) error

// SweepOptions bound one sweep pass.
type SweepOptions struct {
	// Bound is the longest a cycle may stay deferred (clamped by
	// ClampDeferralBound).
	Bound time.Duration
	// MaxCycles and MaxMembers bound the re-checks and the provider reads of one
	// pass (defaults DefaultSweepCycles, DefaultSweepMembers). Members beyond the
	// cap are treated as unverified.
	MaxCycles, MaxMembers int
	Escalate              EscalateFunc
}

// SweepResult counts what one pass did.
type SweepResult struct {
	Dropped, Escalated, Deferred int
	// NewlyRefused names the cycles whose escalation was permanently refused in this
	// pass (for the caller to journal once); Refused names every cycle whose refusal
	// stands, durably, so readiness survives a failover and clears when the cycle
	// resolves.
	NewlyRefused, Refused []string
}

// Sweep re-checks the cycles whose deferral has fallen due. For each: recompute
// the cycle from the seed (no cycle left clears the marker); verify its members
// (closed ones are forgotten, which may dissolve the cycle); a cycle every member
// of which is verified open is escalated; one with members that cannot be verified
// is deferred again, or escalated as unverifiable once it has waited the bound or
// fails permanently. It never holds a cycle without a next due time.
func (s Store) Sweep(ctx context.Context, handle *lease.Handle, opt SweepOptions) (SweepResult, error) {
	var result SweepResult
	if s.Closed == nil || opt.Escalate == nil {
		return result, nil
	}
	bound := ClampDeferralBound(opt.Bound)
	maxCycles, maxMembers := opt.MaxCycles, opt.MaxMembers
	if maxCycles <= 0 {
		maxCycles = DefaultSweepCycles
	}
	if maxMembers <= 0 {
		maxMembers = DefaultSweepMembers
	}
	due, err := s.due(ctx, handle, maxCycles)
	if err != nil {
		return result, err
	}
	// A sweep never waits on a retry: the next pass is the retry.
	once := s
	once.RetryDelays = []time.Duration{}
	var errs []error
	for _, d := range due {
		outcome, err := once.recheck(ctx, handle, d, bound, maxMembers, opt.Escalate)
		if err != nil {
			if isStale(err) {
				return result, err
			}
			if errors.Is(err, ErrEscalationRefused) {
				result.NewlyRefused = append(result.NewlyRefused, d.Key)
			}
			errs = append(errs, fmt.Errorf("re-check deferred cycle %s: %w", d.Key, err))
			continue
		}
		switch outcome {
		case outcomeDropped:
			result.Dropped++
		case outcomeEscalated:
			result.Escalated++
		case outcomeDeferred:
			result.Deferred++
		}
	}
	standing, err := once.reconcileRefused(ctx, handle)
	if err != nil {
		if isStale(err) {
			return result, err
		}
		errs = append(errs, fmt.Errorf("reconcile refused cycles: %w", err))
	}
	result.Refused = standing
	return result, errors.Join(errs...)
}

type sweepOutcome int

const (
	outcomeDropped sweepOutcome = iota + 1
	outcomeEscalated
	outcomeDeferred
)

func isStale(err error) bool {
	return errors.Is(err, lease.ErrStale) || errors.Is(err, lease.ErrSelfFenced)
}

func (s Store) recheck(ctx context.Context, handle *lease.Handle, d Deferral, bound time.Duration, maxMembers int, escalate EscalateFunc) (sweepOutcome, error) {
	for range maxMembers + 1 {
		recs, err := s.snapshot(ctx, handle)
		if err != nil {
			return 0, err
		}
		cycle := blockedrecords.FindCycle(recs, d.Key)
		if len(cycle.Affected) == 0 {
			// The seed, or the cycle through it, is gone: nothing to escalate.
			return outcomeDropped, s.clearDeferral(ctx, handle, d.Key)
		}
		members, overflow := cycle.Affected, []blockedcycle.Node(nil)
		if len(members) > maxMembers {
			members, overflow = members[:maxMembers], members[maxMembers:]
		}
		closed, skipped, unverifiable := s.verifyCycle(ctx, recs, members, d.Record.RunID)
		if len(closed) > 0 {
			if err := s.Forget(ctx, handle, closed); err != nil {
				return 0, err
			}
			continue // the cycle may be gone, or smaller: look again
		}
		skipped = append(skipped, overflow...)
		if len(skipped) == 0 && len(unverifiable) == 0 {
			return outcomeEscalated, s.escalateDeferred(ctx, handle, d, cycle, escalate)
		}
		permanent := slices.ContainsFunc(unverifiable, func(u unverified) bool { return u.Permanent })
		if permanent || d.Age >= bound {
			cycle.Unverified = unverifiedNotes(skipped, unverifiable)
			return outcomeEscalated, s.escalateDeferred(ctx, handle, d, cycle, escalate)
		}
		return outcomeDeferred, s.deferCycle(ctx, handle, d.Key, deferReason(skipped, unverifiable), nextDelay(d.Attempts, bound-d.Age))
	}
	return 0, errors.New("deferred cycle did not settle")
}

// escalateDeferred runs the provider writes and, only once they succeed, clears the
// marker: a crash in between repeats the (idempotent) writes.
func (s Store) escalateDeferred(ctx context.Context, handle *lease.Handle, d Deferral, cycle blockedcycle.Result, escalate EscalateFunc) error {
	if err := escalate(ctx, cycle, d); err != nil {
		if allPermanent(err) {
			// Retrying cannot help: stop, and say so. The members stay parked, but the
			// refusal is latched in the seed row (durable, readiness-visible) and
			// journaled by the caller, not retried quietly.
			return errors.Join(fmt.Errorf("%w: %w", ErrEscalationRefused, err), s.markRefused(ctx, handle, d.Key, refusedClass(err)))
		}
		return err
	}
	// A refusal latched on another seed of this cycle is superseded by this success.
	if err := s.releaseRefused(ctx, handle, cycle.Affected); err != nil {
		return err
	}
	return s.clearDeferral(ctx, handle, d.Key)
}

// allPermanent reports whether every failure inside err is permanent. An
// escalation joins one failure per member write, so a permanent refusal of one
// member must not stop the cycle while another member's failure is still
// transient: that one is retried, and only a cycle whose every failure is
// permanent is refused.
func allPermanent(err error) bool {
	if err == nil {
		return false
	}
	// The type assertions are deliberate: they walk the tree one node at a time,
	// which errors.As/Is (any-match) cannot express.
	if multi, ok := err.(interface{ Unwrap() []error }); ok { //nolint:errorlint // structural walk, not a match
		children := multi.Unwrap()
		return len(children) > 0 && !slices.ContainsFunc(children, func(c error) bool { return !allPermanent(c) })
	}
	if single, ok := err.(interface{ Unwrap() error }); ok { //nolint:errorlint // structural walk, not a match
		if inner := single.Unwrap(); inner != nil {
			return allPermanent(inner)
		}
	}
	return providerwriter.Permanent(err)
}

// nextDelay doubles from FirstDeferralDelay up to MaxDeferralDelay, and never past
// the bound: the last re-check lands at the bound so the cycle ends there.
func nextDelay(attempts int, remaining time.Duration) time.Duration {
	delay := FirstDeferralDelay
	for i := 1; i < attempts && delay < MaxDeferralDelay; i++ {
		delay *= 2
	}
	delay = min(delay, MaxDeferralDelay)
	if remaining > 0 && remaining < delay {
		delay = remaining
	}
	return max(delay, time.Second)
}

func deferReason(skipped []blockedcycle.Node, failed []unverified) string {
	switch {
	case len(failed) > 0:
		return failed[0].Class
	case len(skipped) > 0:
		return "provider read budget spent"
	}
	return ""
}
