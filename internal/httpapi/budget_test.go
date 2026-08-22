package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// TestBudgetExceededIsA503NotA500 pins the mapping correction.
//
// §7.1's fourth pass said "SQLITE_INTERRUPT maps to the 503 path". It cannot:
// modernc.org/sqlite arms the interrupt only when ctx.Done() != nil and then
// rewrites the result to ctx.Err(), so a caller observes
// context.DeadlineExceeded and never a SQLite code (#1935).
//
// Before this, DeadlineExceeded fell through writeReadError's default to a
// 500 read_error — a deliberate shed presented as a server fault, which is
// exactly the "slow and broken are the same event" failure the diagnosis's §5.4
// names.
func TestBudgetExceededIsA503NotA500(t *testing.T) {
	rec := httptest.NewRecorder()
	if !budgetExceeded(rec, context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded was not recognised as a budget expiry")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("no Retry-After header; a shed request must cooperate with client backoff (§13.6)")
	}

	// A wrapped deadline error must still be recognised — read paths wrap.
	rec = httptest.NewRecorder()
	wrapped := errors.Join(errors.New("query gaggle stats"), context.DeadlineExceeded)
	if !budgetExceeded(rec, wrapped) {
		t.Error("a wrapped DeadlineExceeded was not recognised")
	}

	// Client cancellation is NOT a budget expiry: it stays the quiet 499.
	// Conflating them would log every portal abort as a server shed (#1367).
	rec = httptest.NewRecorder()
	if budgetExceeded(rec, context.Canceled) {
		t.Error("context.Canceled was treated as a budget expiry; it is the client giving up, not the server shedding")
	}
}

// TestEveryReadRouteHasABudgetExceptTheStream pins that a route cannot be added
// without a bound, and that the one deliberate exemption is explicit.
//
// §7.1's sixth-pass correction: requiring a non-zero budget for EVERY route is
// actively wrong for /api/v1/events, a deliberately unbounded SSE stream, and
// would force "a fictitious number that the enforcement layer then has to
// special-case anyway". Declaring the exemption is the honest form.
func TestEveryReadRouteHasABudgetExceptTheStream(t *testing.T) {
	for _, route := range apicontract.V1Routes() {
		budget, bounded := routeBudget(route.ID)
		switch route.ID {
		case apicontract.RouteEvents:
			if bounded {
				t.Errorf("%s has a budget of %s; the event stream is unbounded by design and a "+
					"deadline would terminate live updates", route.ID, budget)
			}
		default:
			if !bounded {
				t.Errorf("%s has no budget; every read route must be bounded", route.ID)
				continue
			}
			if budget <= 0 {
				t.Errorf("%s has a non-positive budget %s", route.ID, budget)
			}
			// §7.1: "every server budget is strictly below the client's 10s
			// abort", so the server decides rather than the client. Blob
			// routes are the known defect (apicontract cost_test); the
			// credential plane's resolve route is exempt BY DESIGN — its
			// budget contains an outbound GitHub App mint (30s ceiling) and
			// its only callers are stage pods, never the portal client whose
			// abort this rule is about (apicontract.CredentialResolveBudget).
			if route.ID != apicontract.RouteRunArtifact && route.ID != apicontract.RouteRunTranscript &&
				route.ID != apicontract.RouteCredentialResolve &&
				budget >= clientAbortBackstop {
				t.Errorf("%s budget %s is not strictly below the client's %s abort; the client would "+
					"give up first and the server would keep working",
					route.ID, budget, clientAbortBackstop)
			}
		}
	}
}

// clientAbortBackstop is the portal's client-side abort. Server budgets sit
// below it so it is a backstop rather than the only bound.
const clientAbortBackstop = 10 * time.Second

// TestBudgetDeadlineReachesTheHandler proves the deadline is actually installed
// on the request context a handler sees, rather than merely computed.
func TestBudgetDeadlineReachesTheHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)

	bounded, cancel := withBudget(rec, req, 250*time.Millisecond)
	defer cancel()

	deadline, ok := bounded.Context().Deadline()
	if !ok {
		t.Fatal("no deadline on the request context; the budget was not applied")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 250*time.Millisecond {
		t.Errorf("deadline is %s away, want <= 250ms and positive", remaining)
	}

	// And it must actually fire, which is the whole point.
	select {
	case <-bounded.Context().Done():
		if !errors.Is(bounded.Context().Err(), context.DeadlineExceeded) {
			t.Errorf("context ended with %v, want DeadlineExceeded", bounded.Context().Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the request context never expired")
	}
}
