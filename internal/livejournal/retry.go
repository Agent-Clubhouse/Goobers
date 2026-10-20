package livejournal

import (
	"time"

	"github.com/goobers/goobers/internal/retryutil"
)

// retry.go bounds HTTPEmitter.Emit's transient-failure exposure (#4260).
// Emit's redelivery is already safe: every op
// carries a caller-derived idempotency key the writer dedupes on
// (livejournal.go's Op.Key doc, applyOp), so retrying an identical batch
// after a dropped or refused connection can only be a no-op on the server
// side, never a double-apply.

// RetryPolicy overrides retry pacing for one emitter. Nonpositive values retain the
// production defaults.
type RetryPolicy = retryutil.ClientPolicy

// defaultEmitRetryDeadline bounds Emit's retry loop when the caller sets no
// RetryDeadline of its own. Short relative to the surrender plane's default:
// Emit rides hot paths (a heartbeat tick, a span append) that can run many
// times per stage, and a caller with a tighter context deadline already in
// force (e.g. the pod's 30s-per-heartbeat or 15s blob-write-through budgets)
// wins regardless, since context.WithTimeout always honors the earlier of
// the two.
const defaultEmitRetryDeadline = 60 * time.Second
