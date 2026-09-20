package worktree

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"time"
)

// DefaultCleanupDiscoveryFactor derives a pass's default discovery bound from
// its attempt limit. Discovery only has to find enough pending work to fill the
// pass, plus headroom for candidates that turn out to be ineligible once their
// records are re-read under the lock. Scanning materially further is work the
// pass cannot use.
const DefaultCleanupDiscoveryFactor = 4

// CleanupDeferralReason names why a bounded pass stopped before exhausting its
// attempt limit. It is reported rather than swallowed: #5264 requires that a
// pass report elapsed work and the reason for deferral instead of presenting a
// truncated pass as a completed cleanup.
type CleanupDeferralReason string

const (
	// CleanupDeferralNone means the pass ran to its natural end.
	CleanupDeferralNone CleanupDeferralReason = ""
	// CleanupDeferralDiscoveryLimit means discovery stopped at its bound with
	// pending records left unexamined. Those records are durable and the next
	// pass resumes after this pass's cursor, so they are deferred, not lost.
	CleanupDeferralDiscoveryLimit CleanupDeferralReason = "discovery-limit"
	// CleanupDeferralBudgetDiscovery means the whole-pass budget expired while
	// still discovering candidates.
	CleanupDeferralBudgetDiscovery CleanupDeferralReason = "budget-exhausted-during-discovery"
	// CleanupDeferralBudgetAttempts means the whole-pass budget expired partway
	// through attempting the candidates discovery found.
	CleanupDeferralBudgetAttempts CleanupDeferralReason = "budget-exhausted-during-attempts"
)

// CleanupWarningClass classifies why one candidate did not get cleaned up, so
// an operator is pointed at the right remediation. #5264 requires specifically
// that a file-holder refusal is distinguishable from a recovery-capacity
// refusal and from a malformed durable record: the three need entirely
// different operator actions, and lumping them into one warning list meant the
// only way to tell them apart was to read the wrapped error prose.
//
// Every class is derived from a signal already present in the error chain. In
// particular this does NOT detect which process holds a file — that is an
// explicit #5264 non-goal, and a filesystem-class warning says only that the
// removal itself was refused, not who refused it.
type CleanupWarningClass string

const (
	// CleanupWarningUnknown is the zero value: a warning no rule matched. It is
	// deliberately the default so an unrecognized failure is reported as
	// unclassified rather than mislabeled as one of the specific causes.
	CleanupWarningUnknown CleanupWarningClass = ""
	// CleanupWarningRecord is a durable record that could not be read or whose
	// identity is not canonical. Remediation: inspect/repair the record pair.
	CleanupWarningRecord CleanupWarningClass = "record"
	// CleanupWarningRecoveryCapacity is a custody handoff refused because
	// recovery inventory is full. Remediation: free recovery capacity.
	CleanupWarningRecoveryCapacity CleanupWarningClass = "recovery-capacity"
	// CleanupWarningRecoveryCapture is a custody handoff refused because the
	// recovery git subprocess that captures the durable patch/index failed —
	// a missing ref/object, lock contention, or an unsafe-repository
	// refusal, as distinct from the inventory simply being full. Remediation
	// depends on the class recorded in the wrapped recovery.CaptureError:
	// missing-object and unsafe-repository need operator investigation of
	// the repository itself; locked is transient contention that clears on
	// its own. See docs/guides/retained-implementation.md.
	CleanupWarningRecoveryCapture CleanupWarningClass = "recovery-capture"
	// CleanupWarningHandoff is a custody handoff deferred for any other reason.
	// Remediation: inspect that handoff; the worktree stays owned meanwhile.
	CleanupWarningHandoff CleanupWarningClass = "handoff-deferred"
	// CleanupWarningFilesystem is a removal the filesystem refused — busy,
	// non-empty or permission-denied. Remediation: find what holds the files.
	CleanupWarningFilesystem CleanupWarningClass = "filesystem-refused"
)

// classifyCleanupWarning maps one cleanup failure onto its remediation class.
//
// Order matters, and it is the narrow-to-broad order. A capacity refusal
// arrives wrapped in ErrCleanupDeferred, so testing the specific inventory
// sentinel first is what keeps it from being flattened into the generic
// handoff class; likewise a record fault is checked before the filesystem
// errnos, because an unreadable record surfaces as an I/O error that a
// permission check would otherwise claim.
func classifyCleanupWarning(err error) CleanupWarningClass {
	switch {
	case err == nil:
		return CleanupWarningUnknown
	case errors.Is(err, errCleanupRecordInvalid):
		return CleanupWarningRecord
	case isRecoveryCapacityRefusal(err):
		return CleanupWarningRecoveryCapacity
	case isRecoveryCaptureRefusal(err):
		return CleanupWarningRecoveryCapture
	case errors.Is(err, ErrCleanupDeferred):
		return CleanupWarningHandoff
	case errors.Is(err, syscall.EBUSY), errors.Is(err, syscall.ENOTEMPTY),
		errors.Is(err, fs.ErrPermission):
		return CleanupWarningFilesystem
	default:
		return CleanupWarningUnknown
	}
}

// errCleanupRecordInvalid marks a durable record that cannot be trusted to
// authorize a destructive action. It is a sentinel rather than a prose match so
// the classification cannot drift when the message is reworded.
var errCleanupRecordInvalid = errors.New("worktree: cleanup record is not usable")

// ErrCleanupRecoveryCapacity marks a custody handoff refused because recovery
// inventory is full, as distinct from a handoff that is merely still in
// progress. The two need different operator actions — free capacity versus wait
// — so #5264 requires they be distinguishable.
//
// It lives here, and the recovery guard wiring wraps its own inventory-full
// sentinel in it, because worktree cannot import internal/recovery: recovery
// reaches worktree transitively (recovery -> apicontract -> readservice ->
// creditgraph -> telemetry -> worktree), so that direction is an import cycle.
// Matching recovery's sentinel by message prose instead would be a
// classification that silently breaks the first time the message is reworded,
// so the dependency is inverted rather than faked.
var ErrCleanupRecoveryCapacity = errors.New("worktree cleanup deferred: recovery capacity unavailable")

func isRecoveryCapacityRefusal(err error) bool {
	return errors.Is(err, ErrCleanupRecoveryCapacity)
}

// ErrCleanupRecoveryCapture marks a custody handoff refused because the
// recovery git capture itself failed (a missing ref/object, lock
// contention, or an unsafe-repository refusal), as distinct from the
// inventory being full (ErrCleanupRecoveryCapacity) or the handoff merely
// still being in progress. #5352: without this, every non-capacity recovery
// failure flattened into the generic "handoff deferred" class alongside
// unrelated guards (mutation-receipt handoffs, unknown-base retention),
// hiding the one class an operator can actually act on.
//
// It lives here for the same import-direction reason as
// ErrCleanupRecoveryCapacity: worktree cannot import internal/recovery.
var ErrCleanupRecoveryCapture = errors.New("worktree cleanup deferred: recovery capture failed")

func isRecoveryCaptureRefusal(err error) bool {
	return errors.Is(err, ErrCleanupRecoveryCapture)
}

// passBudget bounds one whole cleanup pass — discovery and attempts together —
// against an injectable clock.
//
// The clock is a seam rather than a direct time.Now call because the budget's
// behavior has to be tested deterministically: a test that proved truncation by
// doing real work for a real duration would be exactly the wall-clock
// assertion that makes a suite flaky under CI contention.
type passBudget struct {
	clock    func() time.Time
	start    time.Time
	deadline time.Time
}

func newPassBudget(clock func() time.Time, limit time.Duration) *passBudget {
	if clock == nil {
		clock = time.Now
	}
	b := &passBudget{clock: clock, start: clock()}
	if limit > 0 {
		b.deadline = b.start.Add(limit)
	}
	return b
}

// exhausted reports whether the pass may no longer start new work. A zero
// deadline means unbounded, which preserves the pre-#5264 behavior exactly for
// any caller that does not set a budget.
func (b *passBudget) exhausted() bool {
	if b.deadline.IsZero() {
		return false
	}
	return !b.clock().Before(b.deadline)
}

func (b *passBudget) elapsed() time.Duration {
	return b.clock().Sub(b.start)
}

// cleanupDiscovery is one bounded discovery scan's outcome.
type cleanupDiscovery struct {
	candidates []cleanupRetryCandidate
	warnings   []ReapWarning
	// examined counts durable records actually READ, which is the I/O the bound
	// exists to cap. Directory entries are enumerated to locate those records
	// and are not counted; see cleanupRetryCandidates for why that distinction
	// is stated rather than glossed.
	examined int
	reason   CleanupDeferralReason
}

func (d *cleanupDiscovery) addWarning(limit int, path string, err error) {
	if len(d.warnings) >= limit {
		return
	}
	d.warnings = append(d.warnings, ReapWarning{
		Path: path, Err: err, Class: classifyCleanupWarning(err),
	})
}

func discoveryLimitFor(opts CleanupRetryOptions) int {
	if opts.DiscoveryLimit > 0 {
		return opts.DiscoveryLimit
	}
	limit := opts.Limit * DefaultCleanupDiscoveryFactor
	if limit < opts.Limit {
		// Overflow on a pathological limit; fall back to the attempt limit,
		// which is always enough to fill the pass.
		return opts.Limit
	}
	return limit
}

func deferralMessage(reason CleanupDeferralReason, examined, attempted int, elapsed time.Duration) string {
	return fmt.Sprintf(
		"worktree: cleanup pass deferred (%s) after examining %d durable record(s) and attempting %d in %s; "+
			"remaining pending records are durable and resume after this pass's cursor",
		reason, examined, attempted, elapsed.Round(time.Millisecond))
}
