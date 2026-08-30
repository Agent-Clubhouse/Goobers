package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/livejournal"
)

type fakeJournalService struct {
	response livejournal.EmitResponse
	err      error
	requests []livejournal.EmitRequest
}

func (f *fakeJournalService) Emit(_ context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	f.requests = append(f.requests, req)
	return f.response, f.err
}

func emitBody(runID string) string {
	return fmt.Sprintf(`{"runId":%q,"gaggle":"web","ops":[{"kind":"append","key":"k1","time":"2026-08-22T00:00:00Z"}]}`, runID)
}

// TestJournalEmitRouteIsInTheContract pins the journal plane onto the §7
// contract discipline: a versioned POST mutation with the mutation budget,
// like the other write planes.
func TestJournalEmitRouteIsInTheContract(t *testing.T) {
	route, ok := apicontract.V1Route(apicontract.RouteJournalEmit)
	if !ok {
		t.Fatal("journalEmit is not in the V1 contract")
	}
	if route.Method != http.MethodPost || route.Path != apicontract.RunJournalEmitPath {
		t.Fatalf("route = %+v", route)
	}
	if route.Cost != apicontract.CostMutation || route.Budget != apicontract.MutationBudget {
		t.Fatalf("route cost/budget = %s/%s, want mutation discipline", route.Cost, route.Budget)
	}
}

func TestJournalEmitDispatchesAndValidates(t *testing.T) {
	service := &fakeJournalService{response: livejournal.EmitResponse{Applied: 1, Deduplicated: 1, Seq: 7}}
	handler := writePlaneHandler(t, nil, AllowAll, WithJournalService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", emitBody("run-1")))
	if response.Code != http.StatusOK {
		t.Fatalf("emit status = %d, body = %s", response.Code, response.Body)
	}
	var decoded livejournal.EmitResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil || decoded.Seq != 7 || decoded.Applied != 1 {
		t.Fatalf("decoded response = %+v, err = %v", decoded, err)
	}
	if got := response.Header().Get(HeaderSourceApplied); got != "run-1:7" {
		t.Fatalf("source-applied header = %q, want run-1:7", got)
	}
	if len(service.requests) != 1 || service.requests[0].RunID != "run-1" {
		t.Fatalf("service saw %+v", service.requests)
	}

	// An omitted body run id is filled from the route.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit",
		`{"gaggle":"web","ops":[{"kind":"append","key":"k2","time":"2026-08-22T00:00:01Z"}]}`))
	if response.Code != http.StatusOK || len(service.requests) != 2 || service.requests[1].RunID != "run-1" {
		t.Fatalf("route-derived run id: status = %d, requests = %+v", response.Code, service.requests)
	}

	// Validation failures never reach the writer.
	before := len(service.requests)
	for name, request := range map[string]*http.Request{
		"body run mismatch":  jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", emitBody("run-2")),
		"no ops":             jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", `{"runId":"run-1","gaggle":"web","ops":[]}`),
		"unknown field":      jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", `{"runId":"run-1","gaggle":"web","surprise":true,"ops":[{"kind":"append","key":"k"}]}`),
		"empty body":         jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", ``),
		"wrong content type": httptest.NewRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", nil),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: status = %d, want 400/415", name, response.Code)
		}
	}
	if len(service.requests) != before {
		t.Fatal("invalid emits reached the journal service")
	}
}

// TestJournalEmitMapsWriterRefusalsToTypedErrors: a terminal journal is a
// conflict the emitter must stop retrying; an unopened run without its open
// header is the caller's error.
func TestJournalEmitMapsWriterRefusalsToTypedErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"terminal", fmt.Errorf("apply op: %w", livejournal.ErrTerminal), http.StatusConflict, "journal_terminal"},
		{"unopened", fmt.Errorf("run x: %w", livejournal.ErrUnknownRun), http.StatusBadRequest, "journal_unopened"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeJournalService{err: tc.err}
			handler := writePlaneHandler(t, nil, AllowAll, WithJournalService(service))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", emitBody("run-1")))
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", response.Code, tc.wantStatus, response.Body)
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil || envelope.Error.Code != tc.wantCode {
				t.Fatalf("error code = %q (err %v), want %q", envelope.Error.Code, err, tc.wantCode)
			}
		})
	}
}

// TestJournalEmitUnavailableWithoutService: a daemon that wires no live
// writer (no engine configured) answers 503, not 404 — the plane exists, the
// instance does not serve it.
func TestJournalEmitUnavailableWithoutService(t *testing.T) {
	handler := writePlaneHandler(t, nil, AllowAll)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", emitBody("run-1")))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

// TestPodPrincipalIsConfinedToItsOwnJournal proves both halves of pod
// containment on the journal plane: RequireRoles admits a pod principal onto
// the emit route, and the handler binds it to its own run.
func TestPodPrincipalIsConfinedToItsOwnJournal(t *testing.T) {
	service := &fakeJournalService{response: livejournal.EmitResponse{Applied: 1, Seq: 1}}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, authenticator, RequireRoles(), WithJournalService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", emitBody("run-1")))
	if response.Code != http.StatusOK {
		t.Fatalf("pod emit into its own run: status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.requests) != 1 {
		t.Fatalf("journal service saw %+v", service.requests)
	}

	// Another run's journal is refused before the writer.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, "/api/v1/runs/run-2/journal/emit", emitBody("run-2")))
	if response.Code != http.StatusForbidden {
		t.Fatalf("pod emit into another run: status = %d, want 403", response.Code)
	}
	if len(service.requests) != 1 {
		t.Fatal("cross-run pod emit reached the journal service")
	}

	// Off-plane methods and paths stay refused for pods: a GET on the emit
	// path has no route at all (405), and the HITL plane stays human-only
	// (403 from RequireRoles).
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/runs/run-1/journal/emit", nil))
	if response.Code != http.StatusMethodNotAllowed && response.Code != http.StatusForbidden {
		t.Errorf("pod GET on emit path: status = %d, want refusal", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, "/api/v1/runs/run-1/escalation/resolve", `{"resolution":"deny"}`))
	if response.Code != http.StatusForbidden {
		t.Errorf("pod POST on HITL plane: status = %d, want 403", response.Code)
	}
}

// TestJournalPlanePathMatchesOnlyTheEmitRoute pins the structural matcher the
// authorizer keys pod admission on.
func TestJournalPlanePathMatchesOnlyTheEmitRoute(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/runs/run-1/journal/emit":       true,
		"/api/v1/runs/run-1/journal":            false,
		"/api/v1/runs//journal/emit":            false,
		"/api/v1/runs/run-1/x/journal/emit":     false,
		"/api/v1/runs/run-1/journal/emit/extra": false,
		"/api/v1/claims/acquire":                false,
	} {
		if got := journalPlanePath(path); got != want {
			t.Errorf("journalPlanePath(%q) = %t, want %t", path, got, want)
		}
	}
}
