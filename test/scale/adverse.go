package main

import "time"

// slowDiskDelay is the per-operation latency a blob-mount simulation injects.
//
// §16.9 asks for a rebuild figure on network- or blob-backed storage because
// §2.6 concludes the rebuild cost driver is **file opens, not bytes** — 29,759
// of them — and on a per-open-latency-dominated mount that decides §13.2's
// whole per-replica-versus-shared question, for which "no defensible figure
// exists".
//
// No such mount is available here, so the figure is produced by injecting a
// fixed per-open delay and is reported as SIMULATED. That is worth having and
// is not the same as measuring: a real mount also has concurrency limits,
// throughput ceilings, and tail latencies this does not model. The number bounds
// the decision; it does not settle it.
const slowDiskDelay = 2 * time.Millisecond

// simulatedOpenLatency returns the wall time a rebuild would additionally cost on
// a mount with the given per-open latency, given a measured open count.
//
// Deliberately a projection from a measured open count rather than a wrapped
// filesystem: wrapping every open would make the harness's own runtime
// prohibitive at 1x (29,759 opens x 2ms = 60s per rebuild, before any real work),
// and the projection is exactly as informative because per-open latency is
// additive and the open count is measured, not estimated.
func simulatedOpenLatency(opens uint64, perOpen time.Duration) time.Duration {
	return time.Duration(opens) * perOpen
}
