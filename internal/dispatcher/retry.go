package dispatcher

import "github.com/goobers/goobers/internal/retryutil"

// retry.go bounds the transient-failure exposure of this package's two
// daemon-facing PUT clients (SurrenderPutClient, BlobClient) — #4260. A brief
// control-plane restart (every rollout, since goobers-api is single-replica
// by construction, #3809) previously turned an already-finished stage
// attempt's single-shot PUT into a lost result on the very first refused or
// dropped connection. Both endpoints are idempotent upserts keyed by the
// caller's own identity (surrender: write-once by run/stage/attempt,
// surrender.go:298-302; blob: write-once by content digest, blob.go:111-113),
// so retrying the identical request is safe by construction — no client-side
// idempotency key is needed, only patience.

// RetryPolicy overrides retry pacing for one client. Nonpositive values retain the
// production defaults.
type RetryPolicy = retryutil.ClientPolicy
