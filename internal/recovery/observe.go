package recovery

import "context"

// FallbackReason is a bounded, closed vocabulary for why WriteSnapshotBundle
// could not select the smaller delta format and fell back to a full,
// self-contained bundle (maintainer decision, #4862; observability, #5028).
type FallbackReason string

const (
	// FallbackReasonNoBaseRef marks a record with no tracked base ref, either
	// pre-#4823 or otherwise never populated.
	FallbackReasonNoBaseRef FallbackReason = "no_base_ref"
	// FallbackReasonBaseUnreachable marks a base commit that is no longer
	// provably an ancestor of its own tracked base ref (a rewritten branch,
	// or a base ref that has since moved past it in a way merge-base
	// --is-ancestor cannot confirm).
	FallbackReasonBaseUnreachable FallbackReason = "base_unreachable"
)

// RestoreFailureReason is a bounded, closed vocabulary for why
// ImportSnapshotBundle failed, labeled instead of the free-text error so it
// can become a metric dimension without leaking repository paths, run ids,
// or error text into a label (#5028).
type RestoreFailureReason string

const (
	// RestoreFailureReasonArchiveInvalid marks a staged archive that failed
	// its own verification: an invalid record, a digest or size mismatch
	// against the trusted record, or a bundle header that does not match the
	// declared format.
	RestoreFailureReasonArchiveInvalid RestoreFailureReason = "archive_invalid"
	// RestoreFailureReasonBaseMissing marks a delta bundle whose required
	// base commit is not present in the unbundling repository.
	RestoreFailureReasonBaseMissing RestoreFailureReason = "base_missing"
	// RestoreFailureReasonImportFailed marks every other restore-path
	// failure: a Git unbundle failure, an imported identity mismatch, or a
	// failure to pin the restored ref.
	RestoreFailureReasonImportFailed RestoreFailureReason = "import_failed"
)

// SnapshotObserver receives operational metadata about recovery bundle
// capture and restore so storage behavior (the delta/full mix, bundle size,
// fallback rate, restore failure rate) can be operated without this package
// depending on any specific telemetry backend (#5028). Implementations must
// be safe for concurrent use. A nil SnapshotObserver disables observation,
// which is the default for every caller that never attaches one.
//
// Reasons are passed as plain strings (the underlying type of FallbackReason
// and RestoreFailureReason), not those named types: internal/telemetry sits
// downstream of internal/recovery in the import graph (through
// internal/apicontract and internal/localscheduler), so an interface an
// observer there implements cannot reference a type this package declares.
type SnapshotObserver interface {
	// SnapshotCaptured records the bundle format selected for one successful
	// capture (archiveFormatFull or archiveFormatDelta) and its emitted size
	// in bytes.
	SnapshotCaptured(format string, bytes int64)
	// SnapshotFallback records why a capture could not use the smaller delta
	// format and fell back to a full bundle. reason is one of the
	// FallbackReason constants.
	SnapshotFallback(reason string)
	// SnapshotRestoreFailed records a restore-path failure. reason is one of
	// the RestoreFailureReason constants.
	SnapshotRestoreFailed(reason string)
}

type snapshotObserverKey struct{}

// WithSnapshotObserver attaches observer to ctx so nested capture/restore
// calls (WriteSnapshotBundle, PublishSnapshotBundle, ImportSnapshotBundle,
// and everything built on them) can report format, size, fallback reason and
// restore failures without an extra parameter threaded through every
// function in those call chains. Passing a nil observer is a no-op: the
// returned context behaves exactly like ctx.
func WithSnapshotObserver(ctx context.Context, observer SnapshotObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, snapshotObserverKey{}, observer)
}

// observeSnapshot returns the SnapshotObserver attached to ctx, or nil when
// none was attached (the default: observation is opt-in per call tree).
func observeSnapshot(ctx context.Context) SnapshotObserver {
	observer, _ := ctx.Value(snapshotObserverKey{}).(SnapshotObserver)
	return observer
}
