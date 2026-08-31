package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

// podscope_test.go is the route-confinement half of Goobers#3897's evidence.
//
// The dispatcher stamps four different bearers into a goobers-CLI stage's
// environment so that a stage subprocess cannot do more than the one plane it
// was handed. That claim is only true if the SERVER enforces it — a scope
// carried in a token nobody checks is a naming convention. This is the check.

// scopedPodRequest builds an authorized request as a pod principal for run
// "run-1" holding exactly the given scopes (none = the unscoped pod token).
func scopedPodRequest(t *testing.T, method, path string, scopes ...string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	return request.WithContext(context.WithValue(request.Context(), principalContextKey{}, Principal{
		Subject: podPrincipalSubject("run-1"),
		Issuer:  PodPrincipalIssuer,
		Scopes:  scopes,
	}))
}

// The table of every pod-reachable route and the single scope that admits it.
// Read this as the confinement contract itself: adding a pod route without
// adding a line here is what TestEveryPodRouteHasAScope refuses.
func podRouteTable() []struct {
	name   string
	method string
	path   string
	scope  string
} {
	return []struct {
		name   string
		method string
		path   string
		scope  string
	}{
		{"claim acquire", http.MethodPost, apicontract.ClaimAcquirePath, ScopeClaims},
		{"claim renew", http.MethodPost, apicontract.ClaimRenewPath, ScopeClaims},
		{"claim release", http.MethodPost, apicontract.ClaimReleasePath, ScopeClaims},
		{"claim settle", http.MethodPost, apicontract.ClaimSettlePath, ScopeClaims},
		{"claim list", http.MethodPost, apicontract.ClaimListPath, ScopeClaims},
		{"claim recover", http.MethodPost, apicontract.ClaimRecoverPath, ScopeClaims},
		{"trigger ingest", http.MethodPost, apicontract.TriggerIngestPath, ScopeState},
		{"credential resolve", http.MethodPost, apicontract.CredentialResolvePath, ScopeCredential},
		{"journal run-phase", http.MethodPost, apicontract.JournalRunPhasePath, ScopeJournal},
		{"journal conflict-touches", http.MethodPost, apicontract.JournalConflictTouchesPath, ScopeJournal},
		{"journal unpushed-work", http.MethodPost, apicontract.JournalUnpushedWorkPath, ScopeJournal},
		{"journal emit", http.MethodPost, "/api/v1/runs/run-1/journal/emit", ScopeJournal},
		{"run events read", http.MethodGet, "/api/v1/runs/run-1/events", ScopeJournal},
		{"run stage attempts read", http.MethodGet, "/api/v1/runs/run-1/stages/build/attempts", ScopeJournal},
		// The artifact CONTENT route. It is content-addressed like the blob
		// plane and yet deliberately NOT on it: the blob bearer reaches any
		// digest the store holds, while this one is contained by the handler
		// to artifacts the caller's OWN run journal recorded. A blob-scoped
		// bearer reaching here would erase that difference.
		{"run artifact read", http.MethodGet, "/api/v1/runs/run-1/artifacts/sha256:abc", ScopeJournal},
		{"surrender", http.MethodPost, "/api/v1/runs/run-1/stages/build/attempts/1/surrender", ScopeSurrender},
		{"blob read", http.MethodGet, "/api/v1/blobs/sha256:abc", ScopeBlob},
		{"blob write", http.MethodPut, "/api/v1/blobs/sha256:abc", ScopeBlob},
		{"state read", http.MethodGet, "/api/v1/gaggles/alpha/state/claims.json", ScopeState},
		{"state write", http.MethodPut, "/api/v1/gaggles/alpha/state/claims.json", ScopeState},
		{"telemetry stats", http.MethodGet, apicontract.TelemetryStatsPath, ScopeTelemetry},
	}
}

// The headline confinement property: for every pod route, the ONE scope that
// admits it admits it, and every OTHER scope is refused. Written as the full
// cross product rather than a handful of spot checks, because the interesting
// failure is a single miscategorised route — the journal bearer that also
// happens to reach surrender, say — and a spot check would not find it.
func TestPodScopesAreConfinedToTheirOwnPlane(t *testing.T) {
	authorizer := RequireRoles()
	for _, route := range podRouteTable() {
		t.Run(route.name, func(t *testing.T) {
			for _, scope := range knownPodScopes() {
				err := authorizer.Authorize(scopedPodRequest(t, route.method, route.path, scope))
				admitted := err == nil
				if want := scope == route.scope; admitted != want {
					if want {
						t.Errorf("a %s-scoped bearer was REFUSED at %s %s: %v", scope, route.method, route.path, err)
					} else {
						t.Errorf("a %s-scoped bearer was ADMITTED at %s %s, which is the %s plane", scope, route.method, route.path, route.scope)
					}
				}
			}
		})
	}
}

// The single most important line of the whole change, stated on its own so it
// cannot be lost in a table: none of the four bearers the dispatcher hands a
// stage subprocess can surrender that stage's own result. If it could, a
// workflow-authored stage could declare itself successful without doing the
// work, and every downstream gate would believe it.
func TestNoStagePlaneBearerCanSurrender(t *testing.T) {
	authorizer := RequireRoles()
	surrender := "/api/v1/runs/run-1/stages/build/attempts/1/surrender"
	for _, scope := range []string{ScopeClaims, ScopeState, ScopeJournal, ScopeTelemetry} {
		if err := authorizer.Authorize(scopedPodRequest(t, http.MethodPost, surrender, scope)); err == nil {
			t.Errorf("a %s-scoped stage bearer was admitted at the surrender route", scope)
		}
	}
	// The unscoped pod token — held by __dispatch-exec itself, never exported
	// into a stage subprocess — still can, because surrendering the envelope
	// is the wrapper's whole job.
	if err := authorizer.Authorize(scopedPodRequest(t, http.MethodPost, surrender)); err != nil {
		t.Fatalf("the unscoped pod token must still surrender: %v", err)
	}
}

// A scoped bearer is not a skeleton key for the routes NO pod may reach.
func TestScopedPodBearersStillCannotLeaveThePodPlanes(t *testing.T) {
	authorizer := RequireRoles()
	for _, path := range []string{
		"/api/v1/runs",
		"/api/v1/runs/run-1/escalation/resolve",
		"/api/v1/runs/run-1/stages/build/override",
		"/api/v1/gaggles",
	} {
		for _, scope := range append(knownPodScopes(), "") {
			scopes := []string{scope}
			if scope == "" {
				scopes = nil
			}
			if err := authorizer.Authorize(scopedPodRequest(t, http.MethodPost, path, scopes...)); err == nil {
				t.Errorf("pod principal with scopes %v was admitted at %s, which is not a pod plane", scopes, path)
			}
		}
	}
}

// Method confinement: the read planes are reads. A state bearer cannot POST,
// a telemetry bearer cannot write, and a journal bearer cannot turn the
// own-run read route into a write.
func TestPodPlaneMethodsAreConfined(t *testing.T) {
	authorizer := RequireRoles()
	for _, tc := range []struct {
		scope  string
		method string
		path   string
	}{
		{ScopeState, http.MethodPost, "/api/v1/gaggles/alpha/state/claims.json"},
		{ScopeState, http.MethodDelete, "/api/v1/gaggles/alpha/state/claims.json"},
		{ScopeTelemetry, http.MethodPost, apicontract.TelemetryStatsPath},
		{ScopeJournal, http.MethodPost, "/api/v1/runs/run-1/events"},
		{ScopeJournal, http.MethodDelete, "/api/v1/runs/run-1/journal/emit"},
		{ScopeBlob, http.MethodDelete, "/api/v1/blobs/sha256:abc"},
		{ScopeClaims, http.MethodGet, apicontract.ClaimAcquirePath},
	} {
		t.Run(tc.scope+" "+tc.method, func(t *testing.T) {
			if err := authorizer.Authorize(scopedPodRequest(t, tc.method, tc.path, tc.scope)); err == nil {
				t.Errorf("%s %s was admitted for a %s bearer", tc.method, tc.path, tc.scope)
			}
		})
	}
}

// A bearer carrying an unknown scope reaches nothing. This is the fail-closed
// direction for a version-skewed peer: an old daemon meeting a scope name it
// has never heard of must refuse, not shrug and admit.
func TestUnknownPodScopeAdmitsNothing(t *testing.T) {
	authorizer := RequireRoles()
	for _, route := range podRouteTable() {
		if err := authorizer.Authorize(scopedPodRequest(t, route.method, route.path, "goobers:not-a-plane")); err == nil {
			t.Errorf("an unknown scope was admitted at %s %s", route.method, route.path)
		}
	}
}

// The unscoped token is the compatibility path and must stay total: an
// un-redeployed dispatcher still mints unscoped pod tokens, and the day the
// daemon rolls forward first, those tokens must keep working.
func TestUnscopedPodTokenReachesEveryPodPlane(t *testing.T) {
	authorizer := RequireRoles()
	for _, route := range podRouteTable() {
		if err := authorizer.Authorize(scopedPodRequest(t, route.method, route.path)); err != nil {
			t.Errorf("the unscoped pod token was refused at %s %s: %v", route.method, route.path, err)
		}
	}
}

// Nothing in the table may reference a scope this package does not know, and
// every known scope must appear in the table — otherwise a bearer exists that
// opens nothing, or a plane exists no bearer can be minted for. (The
// restatement is pinned to podauth's originals from the other side, in
// internal/podauth's TestPodPlaneScopesMatchPodauth: podauth imports this
// package, so only podauth can compare the two.)
func TestEveryPodScopeOpensExactlyItsOwnRoutes(t *testing.T) {
	known := knownPodScopes()
	inTable := map[string]bool{}
	for _, route := range podRouteTable() {
		if !contains(known, route.scope) {
			t.Errorf("route %s requires scope %q, which is not a known scope", route.name, route.scope)
		}
		inTable[route.scope] = true
	}
	for _, scope := range known {
		if !inTable[scope] {
			t.Errorf("scope %q is mintable but opens no route in the table", scope)
		}
	}
}

// knownPodScopes is every scope a pod bearer can carry, in a stable order.
func knownPodScopes() []string {
	return []string{ScopeClaims, ScopeState, ScopeJournal, ScopeTelemetry, ScopeSurrender, ScopeBlob, ScopeCredential}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// The scope names are wire format: they are signed into a bearer by one
// process and read by another, so a rename is a compatibility break for every
// token already in flight. Pinned literally, and to lowercase — the signed
// payload carries them as a comma-separated list and a comma or a "." in a
// scope name would collide with the payload's own delimiters.
func TestPodScopeNamesArePinnedWireFormat(t *testing.T) {
	for name, want := range map[string]string{
		"ScopeClaims":     "claims",
		"ScopeState":      "state",
		"ScopeJournal":    "journal",
		"ScopeTelemetry":  "telemetry",
		"ScopeSurrender":  "surrender",
		"ScopeBlob":       "blob",
		"ScopeCredential": "credential",
	} {
		var got string
		switch name {
		case "ScopeClaims":
			got = ScopeClaims
		case "ScopeState":
			got = ScopeState
		case "ScopeJournal":
			got = ScopeJournal
		case "ScopeTelemetry":
			got = ScopeTelemetry
		case "ScopeSurrender":
			got = ScopeSurrender
		case "ScopeBlob":
			got = ScopeBlob
		case "ScopeCredential":
			got = ScopeCredential
		}
		if got != want {
			t.Errorf("%s = %q, want %q (renaming it invalidates every bearer in flight)", name, got, want)
		}
	}
	for _, scope := range knownPodScopes() {
		if strings.ContainsAny(scope, ",.") || scope != strings.ToLower(scope) {
			t.Errorf("scope %q contains a payload delimiter or uppercase; it cannot round-trip a signed token", scope)
		}
	}
}
