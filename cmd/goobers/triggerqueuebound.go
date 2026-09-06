package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

// Write-side idempotency and a total safety cap for the pending-trigger
// queue (#4326) — the half the sweep-side bounds above deliberately do not
// cover.
//
// WHAT IS ALREADY DEFENDED. sweepPendingTriggers already bounds DISPATCH:
// maxOutstandingTriggerRequestsPerIdentity refuses more than five same-identity
// requests per pass, and maxTriggerSweepEntriesPerCycle keeps a large backlog
// from stalling the delegation ticker. Those hold unconditionally, including
// for request files dropped straight into the directory — which is what the
// incident's automation did — and they are the reason a flood no longer
// starves the control plane (#4323).
//
// WHAT THEY DO NOT DO. They bound what is DISPATCHED, not what ACCUMULATES,
// and they cannot make a submission idempotent, because by the time the sweep
// sees two same-identity files the duplicate already exists. Two of #4326's
// acceptance criteria live entirely on the write side:
//
//   - "Repeated executions with unchanged daemon state do not create
//     additional trigger requests." A repeat submission still minted a fresh
//     id from os.CreateTemp, so N submissions of one logical ask became N
//     files. The sweep then discarded the excess — correct, but the queue had
//     already grown, and the producer got no signal that it was duplicating.
//   - "A timed-out submission is treated as ambiguous and is reconciled
//     before retrying." A caller whose submission timed out could not express
//     "this is the same ask"; retrying minted a second request for work that
//     may already be queued.
//
// WHY A KEY BECOMES A FILENAME rather than a field the writer checks. A
// check-then-write ("scan for a live request with this key, write if absent")
// races: two producers can both scan, both miss, and both write. Deriving the
// FILENAME from the key instead makes the existing atomic publish do the
// de-duplication — two racing writers name the same path, and the rename
// leaves exactly one file. No locking, no scan, no window.
//
// Keyed naming applies to fire-and-forget (priority) requests only. A
// delegated request has a caller blocked on <id>.response.json, and collapsing
// two of those onto one id would let one waiter consume the other's response;
// those keep unique ids and are bounded by the total cap alone.

// maxPendingTriggerRequests is the total outstanding-request safety cap, the
// backstop beneath the two per-sweep bounds above.
//
// It catches the case they cannot: a flood spread across MANY DISTINCT
// identities. maxOutstandingTriggerRequestsPerIdentity only ever compares
// requests within one identity group, so a producer varying its target
// stays under it forever while the directory grows without limit.
//
// The number is taken from the incident's measured ends, not from taste.
// Legitimate outstanding depth is small — concurrent `goobers run`
// delegations plus internal priority re-ticks, tens at the very most, each
// consumed within a sweep interval. The observed pathological depths were
// 1,177 and 6,000+, and it was around the former that the scheduler heartbeat
// starved (#4323). 256 sits an order of magnitude above any legitimate peak
// and well below the depth that took the control plane down, so it cannot
// refuse real work and cannot be reached without a producer defect. Var, not
// const, so a test can shrink it — matching the bounds above.
var maxPendingTriggerRequests = 256

// pendingTriggerWarnThreshold is the depth at which the daemon starts SAYING
// the queue is deep, well before the cap refuses anything.
//
// Two signals already exist and neither covers this. The sweep journals its
// per-identity bounded-out decisions, but that only fires once ONE identity
// exceeds five, so a queue made deep by many DISTINCT identities is silent
// under it. `goobers status` reports depth and oldest age via
// pendingTriggerQueueStats, but that is PULL — an operator has to think to run
// it, which for 59 hours nobody did.
//
// This is the push half: the daemon says the queue is deep, in the instance
// log, without being asked.
var pendingTriggerWarnThreshold = maxPendingTriggerRequests / 4

// keyedRequestPrefix marks a request id derived from an idempotency key.
// os.CreateTemp-minted ids are digits, so the prefix cannot collide with one.
const keyedRequestPrefix = "k-"

// triggerRequestIdempotencyKey builds the stable key for a fire-and-forget
// re-tick. Same gaggle, same workflow, same source run means the same ask:
// re-evaluate this workflow now. Collapsing repeats of it is not a shortcut,
// it is the same missed-tick policy the scheduler already applies to its own
// cron evaluation — however many fires were requested, one re-evaluation
// answers all of them, because the second would observe the state the first
// already acted on.
//
// THE KEY SCOPES TO OUTSTANDING WORK, NOT TO ALL TIME. Once the sweep has
// consumed a request its file is gone, so the next submission of the same key
// creates a fresh request and dispatches normally. That is the intended
// boundary: collapsing repeats of an ask that is still queued is
// de-duplication, whereas refusing an ask because an identical one was
// answered an hour ago would be silently dropping real work.
func triggerRequestIdempotencyKey(gaggle, workflow, sourceRun string) string {
	return strings.Join([]string{gaggle, workflow, sourceRun}, "\x00")
}

// keyedRequestID maps an idempotency key onto the request id, and therefore
// onto the filename two racing producers will both target.
func keyedRequestID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return keyedRequestPrefix + hex.EncodeToString(sum[:])[:32]
}

// countPendingTriggerRequests reports how many request files are outstanding.
// A directory that does not exist yet is zero, not an error: the first
// submission creates it.
func countPendingTriggerRequests(reqDir string) (int, error) {
	entries, exists, err := readDirectory(reqDir)
	if !exists {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), requestSuffix) {
			count++
		}
	}
	return count, nil
}

// errTriggerQueueFull is the named refusal a submission gets at the cap.
type errTriggerQueueFull struct {
	depth int
	cap   int
	dir   string
}

func (e *errTriggerQueueFull) Error() string {
	return fmt.Sprintf("delegate: refusing to submit: %d trigger requests are already outstanding, at the safety cap of %d "+
		"(#4326). This cap is reached only by a producer that submits without accounting for what is already pending; a caller "+
		"retrying a timed-out submission should reuse its idempotency key rather than submit again. Inspect the queue at %s "+
		"before raising the cap — a deep queue is a symptom, and draining it by widening the bound reproduces the incident the "+
		"cap exists to stop",
		e.depth, e.cap, e.dir)
}

// admitTriggerSubmission enforces the safety cap at the one point every
// supported submission funnels through.
//
// A KEYED request is admitted even at the cap when its own file already
// exists: re-submitting an outstanding request is a no-op on a queue that is
// already at its bound, and refusing it would make the idempotent path fail
// exactly when idempotency matters most — under the backlog it exists to
// prevent.
// The count-then-write is not atomic, so concurrent submitters can overshoot
// the cap by their own concurrency — a handful, against a bound of hundreds.
// That is deliberate: a lock here would serialize every submission to protect
// a number whose whole purpose is to be an order of magnitude away from
// anything legitimate, and the sweep bounds what is DISPATCHED regardless.
func admitTriggerSubmission(reqDir, requestID string) error {
	depth, err := countPendingTriggerRequests(reqDir)
	if err != nil {
		return fmt.Errorf("delegate: count pending trigger requests: %w", err)
	}
	if depth < maxPendingTriggerRequests {
		return nil
	}
	if requestID != "" {
		if _, statErr := os.Stat(filepath.Join(reqDir, requestID+requestSuffix)); statErr == nil {
			return nil
		}
	}
	return &errTriggerQueueFull{depth: depth, cap: maxPendingTriggerRequests, dir: reqDir}
}

// reportPendingTriggerDepth journals the queue depth when it is deep enough to
// mean something, so a runaway producer is visible from the instance log
// rather than only from a directory listing an operator thought to run.
//
// It reports on the sweep, not on submission, because the producer that needs
// catching is precisely the one not using the supported path: the incident's
// automation wrote request files directly. The daemon sees those; the
// submission path never does.
func reportPendingTriggerDepth(log *journal.InstanceLog, depth int) {
	if log == nil || depth < pendingTriggerWarnThreshold {
		return
	}
	code := "trigger_queue_deep"
	message := fmt.Sprintf("%d pending trigger requests outstanding (warn threshold %d, safety cap %d)",
		depth, pendingTriggerWarnThreshold, maxPendingTriggerRequests)
	if depth >= maxPendingTriggerRequests {
		code = "trigger_queue_at_cap"
		message = fmt.Sprintf("%d pending trigger requests outstanding, at or above the safety cap of %d; "+
			"further submissions through the supported path are being refused", depth, maxPendingTriggerRequests)
	}
	_ = log.Append(journal.Event{
		Type: journal.EventError,
		Name: "pending-triggers",
		Error: &journal.ErrorDetail{
			Code:    code,
			Message: message,
		},
		Runner: map[string]any{
			"pendingTriggerRequests": depth,
			"warnThreshold":          pendingTriggerWarnThreshold,
			"safetyCap":              maxPendingTriggerRequests,
		},
	})
}
