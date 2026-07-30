package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// Per-request budgets (#1917, design §7.1).
//
// # Why not http.Server.WriteTimeout
//
// The design originally specified `context.WithTimeout` in the router *and*
// `http.Server.WriteTimeout`. The second half is wrong twice over, and #1935
// records the correction.
//
// WriteTimeout is a single **server-global** field that net/http applies once
// per connection at request start, so a per-route budget has no way to reach it.
// And setting it at all would terminate the SSE stream, which is a deliberately
// unbounded response — the portal's live updates would simply stop at the
// deadline.
//
// The per-request primitive is `http.ResponseController.SetWriteDeadline`, which
// this package already uses in the event stream. So a budget is: a context
// deadline for the work, a write deadline for the socket, both per route, and no
// global WriteTimeout.
//
// # Why a budget is not enough on its own
//
// A deadline only helps if the work observes it. Before the context-threading
// change that preceded this, `internal/telemetry/rollup` had no
// `context.Context` in any non-test signature and the repository had zero
// `QueryContext` call sites — so a router deadline would have expired while the
// query ran happily to completion. Budgets are meaningful here only because that
// landed first.
//
// # Wave 1 sizing
//
// §17 Wave 1.5 says budgets are "generous here, tightened in Wave 4 from
// measured p99.9". These are deliberately loose: the goal now is that no request
// can hang, not that every request is fast. Wave 4 (#1926) replaces them with
// declared per-route costs from §16's measurements.

// Budgets, sized against the Wave 0 baseline at 1x and rounded up generously.
//
// For reference, measured at 1x on the reference host BEFORE Wave 1 landed:
// bounded list pages ran 16-36ms p99, run detail 0.44ms, and the inventory
// surfaces 4.06s p50 / 11.26s p99. After #1741 the inventory surfaces serve from
// a background sample in microseconds. The budgets below therefore sit far above
// steady state and exist to bound pathology.
const (
	// defaultRouteBudget applies to every read route without a specific budget.
	//
	// Strictly below the portal's 10s client abort (§7.1: "every server budget is
	// strictly below the client's 10s abort, which becomes a backstop rather than
	// the only bound"), so the server is the thing that decides.
	defaultRouteBudget = 8 * time.Second

	// artifactBudget covers routes that stream bytes to the client, where wall
	// time is dominated by link speed and payload size rather than server work.
	//
	// §7.1 (sixth pass) notes a body cannot be turned into a 503 after
	// WriteHeader(200), so these are budgeted generously and the real protection
	// is the write deadline, which resets as bytes flow.
	artifactBudget = 60 * time.Second
)

// routeBudget returns the wall-clock budget for a route, and whether the route
// has one at all.
//
// The event stream deliberately has none. It is an unbounded response by
// design, and §7.1's sixth-pass correction records that requiring a non-zero
// budget for every route would force "a fictitious number that the enforcement
// layer then has to special-case anyway". Declaring the exemption here is the
// honest form of that.
func routeBudget(id apicontract.RouteID) (time.Duration, bool) {
	switch id {
	case apicontract.RouteEvents:
		return 0, false
	case apicontract.RouteRunArtifact, apicontract.RouteRunTranscript:
		return artifactBudget, true
	default:
		return defaultRouteBudget, true
	}
}

// withBudget bounds one request's work and its socket writes.
//
// The write deadline is set to the budget plus a margin rather than the budget
// itself: the handler may legitimately still be writing a large body as its
// context expires, and cutting the socket at exactly the same instant would turn
// a complete response into a truncated one.
func withBudget(w http.ResponseWriter, request *http.Request, budget time.Duration) (*http.Request, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(request.Context(), budget)
	// Best-effort: a ResponseWriter that does not support deadlines (a test
	// recorder, a wrapping middleware) simply does not get one, which is a
	// weaker bound rather than a broken request.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(budget + writeDeadlineMargin))
	return request.WithContext(ctx), cancel
}

// writeDeadlineMargin is how much longer the socket may live than the work.
const writeDeadlineMargin = 5 * time.Second

// budgetExceeded reports whether err is this request's budget expiring, and if
// so writes the 503.
//
// # The discriminant, and why it is not SQLITE_INTERRUPT
//
// §7.1's fourth pass said "SQLITE_INTERRUPT maps to the 503 path". It cannot:
// modernc.org/sqlite arms the interrupt only when ctx.Done() != nil and then
// **rewrites the result to ctx.Err()** before returning, so a caller observes
// context.DeadlineExceeded and never a SQLite code. #1935 records the
// correction; this is where it takes effect.
//
// The distinction from context.Canceled matters and is not cosmetic. Canceled is
// the client giving up — already handled as a quiet 499, because on a busy
// instance the portal aborts and re-fires reads faster than the daemon answers
// (#1367) and logging those as errors buries real failures. DeadlineExceeded is
// the SERVER deciding it will not finish, which is a 503 the client should back
// off from. Before this, DeadlineExceeded fell through to a 500 read_error —
// indistinguishable from a genuine fault.
func budgetExceeded(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Retry-After cooperates with client backoff (§13.6) rather than inviting an
	// immediate retry into the same overload.
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	writeError(w, http.StatusServiceUnavailable, "request_budget_exceeded",
		"the request exceeded its server-side time budget")
	return true
}

// retryAfterSeconds is the hint returned with a shed request.
const retryAfterSeconds = 2
