package apicontract

import (
	"net/http"
	"testing"
	"time"
)

// clientAbort is the portal's own request timeout.
//
// Every server budget must be strictly below it, so the SERVER is what decides a
// request has run too long. If a budget met or exceeded it, the client would
// abort first and the user would see a generic network failure instead of the
// 503 + Retry-After the server would have sent — the same outcome, with none of
// the information.
const clientAbort = 10 * time.Second

// TestEveryRouteIsClassified is #1926's central acceptance criterion: "a route
// without a class and a non-zero budget fails a contract test".
//
// This is how "no path is unclassified, and the classification is enforced
// rather than documented" becomes true. Before this, the budget was a switch
// statement in internal/httpapi with a `default` arm — so a route added to the
// contract silently inherited 8 seconds, and nobody had to decide anything.
func TestEveryRouteIsClassified(t *testing.T) {
	for _, route := range V1Routes() {
		if route.Cost == "" {
			t.Errorf("route %s (%s %s) has no cost class; add one deliberately rather than "+
				"letting it inherit a default", route.ID, route.Method, route.Path)
			continue
		}
		if !knownCostClass(route.Cost) {
			t.Errorf("route %s declares unknown cost class %q", route.ID, route.Cost)
		}

		// Stream is the only class permitted a zero budget, and it is REQUIRED to
		// have one: a deadline on an SSE response cuts the stream mid-flight,
		// which a client cannot distinguish from the server dying.
		if route.Cost == CostStream {
			if route.Budget != 0 {
				t.Errorf("route %s is a stream but carries a %s budget; a deadline on an SSE "+
					"response cuts the stream mid-flight", route.ID, route.Budget)
			}
			continue
		}
		if route.Budget <= 0 {
			t.Errorf("route %s (%s) has no budget; only stream routes may omit one",
				route.ID, route.Cost)
		}
		// Blob routes are exempt, and the exemption is a KNOWN DEFECT rather
		// than a design choice — see TestBlobBudgetsExceedTheClientAbort below.
		if route.Cost == CostBlob {
			continue
		}
		if route.Budget >= clientAbort {
			t.Errorf("route %s has a %s budget, at or above the client's %s abort; the client "+
				"would give up first and the user would see a generic network error instead "+
				"of a 503 with Retry-After", route.ID, route.Budget, clientAbort)
		}
	}
}

// TestMutationsAreNotClassifiedAsReads pins that the cost class and the action
// class cannot drift apart.
//
// They answer different questions — what it costs, and whether it writes — but a
// mutation classified as a bounded read would be pooled with list traffic and
// shed under read load, which is the wrong policy for a user pressing Approve.
func TestMutationsAreNotClassifiedAsReads(t *testing.T) {
	for _, route := range V1Routes() {
		// Workflow-execution routes joined the write surface with the §7
		// planes (claims, trigger ingestion): they write runtime state and
		// must be pooled as mutations, not shed with read traffic.
		isMutationAction := route.ActionClass == ActionRuntimeMutation ||
			route.ActionClass == ActionMaintenance ||
			route.ActionClass == ActionWorkflowExecution
		switch {
		case isMutationAction && route.Cost != CostMutation:
			t.Errorf("route %s is a mutation but carries cost class %q; it would be "+
				"pooled and shed as though it were read traffic", route.ID, route.Cost)
		case !isMutationAction && route.Cost == CostMutation:
			t.Errorf("route %s is classified as a mutation but its action class is %q",
				route.ID, route.ActionClass)
		}
	}
}

// TestBlobRoutesCarryTheLargerBudget pins that the two classes whose budgets
// differ actually differ.
//
// A blob route's time is transfer, not query: a large artifact over a slow link
// legitimately takes longer than any query should. If they collapsed to one
// value, either artifacts would be cut off mid-download or every list would be
// allowed a minute.
func TestBlobRoutesCarryTheLargerBudget(t *testing.T) {
	var sawBlob, sawBounded bool
	for _, route := range V1Routes() {
		switch route.Cost {
		case CostBlob:
			sawBlob = true
			if route.Budget != BlobBudget {
				t.Errorf("blob route %s carries budget %s, want %s", route.ID, route.Budget, BlobBudget)
			}
		case CostBounded:
			sawBounded = true
			if route.Budget != BoundedBudget {
				t.Errorf("bounded route %s carries budget %s, want %s", route.ID, route.Budget, BoundedBudget)
			}
		}
	}
	if !sawBlob || !sawBounded {
		t.Fatalf("the fixture no longer covers both classes (blob=%v bounded=%v); this test "+
			"would pass vacuously", sawBlob, sawBounded)
	}
	if BlobBudget <= BoundedBudget {
		t.Errorf("the blob budget (%s) is not larger than the bounded budget (%s); either "+
			"artifacts get cut off mid-download or every list is allowed a minute",
			BlobBudget, BoundedBudget)
	}
}

// TestOnlyOneStreamRoute pins the assumption the zero-budget exemption rests on.
//
// The exemption is safe because exactly one route is a stream and it is the SSE
// endpoint. If a second appeared, "streams have no budget" would silently become
// a policy applied to something nobody reasoned about.
func TestOnlyOneStreamRoute(t *testing.T) {
	var streams []RouteID
	for _, route := range V1Routes() {
		if route.Cost == CostStream {
			streams = append(streams, route.ID)
		}
	}
	if len(streams) != 1 || streams[0] != RouteEvents {
		t.Errorf("stream routes = %v, want exactly [%s]; the zero-budget exemption applies to "+
			"whatever is in this list, so it must stay deliberate", streams, RouteEvents)
	}
}

// TestGetRoutesAreNotMutations is a sanity check that the two classifications
// agree with the HTTP method, which is the third independent statement of the
// same fact.
func TestGetRoutesAreNotMutations(t *testing.T) {
	for _, route := range V1Routes() {
		if route.Method == http.MethodGet && route.Cost == CostMutation {
			t.Errorf("route %s is a GET classified as a mutation", route.ID)
		}
		if route.Method == http.MethodPost && route.Cost != CostMutation {
			t.Errorf("route %s is a POST classified as %q", route.ID, route.Cost)
		}
	}
}

func knownCostClass(class CostClass) bool {
	switch class {
	case CostBounded, CostSingleRun, CostAggregate, CostBlob, CostStream, CostMutation:
		return true
	}
	return false
}

// TestBlobBudgetsExceedTheClientAbort documents a real mismatch rather than
// asserting it away.
//
// §7.1 states that every server budget is strictly below the portal's 10s client
// abort, so the SERVER decides a request has run too long and can answer with a
// 503 the client can act on. That holds for every class except blob.
//
// Artifact and transcript routes carry a 60s budget because their time is
// transfer rather than query — a large artifact over a slow link legitimately
// takes longer than any query should. But the portal fetches them through the
// same client as everything else (portal/src/api/httpClient.ts, DEFAULT_TIMEOUT_MS
// = 10_000), with no per-request override. So the last 50 seconds of that budget
// are unreachable: the client has already aborted, and the server goes on
// working on a response nobody will read.
//
// This test asserts the mismatch EXISTS so it cannot be quietly forgotten. It is
// not fixable here: the fix is a longer per-request timeout on the portal's blob
// fetches, which lands with the client work (#1928/#1930) — those files conflict
// with any other portal branch, so doing it here would collide for no gain.
//
// When the portal gains a blob-specific timeout, this test should FAIL, and the
// correct response is to delete it and remove the exemption above.
func TestBlobBudgetsExceedTheClientAbort(t *testing.T) {
	if BlobBudget <= clientAbort {
		t.Errorf("BlobBudget (%s) is now within the client abort (%s). If the portal gained a "+
			"longer timeout for blob fetches, delete this test and drop the CostBlob "+
			"exemption in TestEveryRouteIsClassified — the general rule now holds.",
			BlobBudget, clientAbort)
	}
	t.Logf("known mismatch: blob routes budget %s against a %s client abort; the trailing %s "+
		"is unreachable until the portal gains a per-request timeout for downloads",
		BlobBudget, clientAbort, BlobBudget-clientAbort)
}
