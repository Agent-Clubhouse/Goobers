package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type surrenderPut struct {
	runID, stage string
	attempt      int
	data         []byte
}

type fakeSurrenderService struct {
	err  error
	puts []surrenderPut
}

func (f *fakeSurrenderService) Put(_ context.Context, runID, stage string, attempt int, data []byte) error {
	f.puts = append(f.puts, surrenderPut{runID: runID, stage: stage, attempt: attempt, data: append([]byte(nil), data...)})
	return f.err
}

func surrenderPath(run, stage string, attempt int) string {
	return "/api/v1/runs/" + run + "/stages/" + stage + "/attempts/" + strconv.Itoa(attempt) + "/surrender"
}

func surrenderedBody(status string) string {
	return `{"result":{"status":"` + status + `"}}`
}

// TestStageSurrenderRouteIsInTheContract pins the surrender plane onto the
// §7 contract discipline, like the journal plane.
func TestStageSurrenderRouteIsInTheContract(t *testing.T) {
	route, ok := apicontract.V1Route(apicontract.RouteStageSurrender)
	if !ok {
		t.Fatal("stageSurrender is not in the V1 contract")
	}
	if route.Method != http.MethodPost || route.Path != apicontract.RunStageSurrenderPath {
		t.Fatalf("route = %+v", route)
	}
	if route.Cost != apicontract.CostMutation || route.Budget != apicontract.MutationBudget {
		t.Fatalf("route cost/budget = %s/%s, want mutation discipline", route.Cost, route.Budget)
	}
}

func TestStageSurrenderDispatchesAndValidates(t *testing.T) {
	service := &fakeSurrenderService{}
	handler := writePlaneHandler(t, nil, AllowAll, WithSurrenderService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, surrenderPath("run-1", "probe-builtin", 1), surrenderedBody("success")))
	if response.Code != http.StatusOK {
		t.Fatalf("surrender status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.puts) != 1 {
		t.Fatalf("service saw %d puts, want 1", len(service.puts))
	}
	put := service.puts[0]
	if put.runID != "run-1" || put.stage != "probe-builtin" || put.attempt != 1 {
		t.Fatalf("put identity = %+v", put)
	}
	var decoded struct {
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(put.data, &decoded); err != nil || decoded.Result.Status != "success" {
		t.Fatalf("stored body decode = %+v, err = %v", decoded, err)
	}

	// Validation failures never reach the plane.
	before := len(service.puts)
	for name, request := range map[string]*http.Request{
		"no status":           jsonRequest(http.MethodPost, surrenderPath("run-1", "probe-builtin", 1), `{"result":{}}`),
		"empty body":          jsonRequest(http.MethodPost, surrenderPath("run-1", "probe-builtin", 1), ``),
		"bad attempt":         jsonRequest(http.MethodPost, surrenderPath("run-1", "probe-builtin", 0), surrenderedBody("success")),
		"non-numeric attempt": jsonRequest(http.MethodPost, "/api/v1/runs/run-1/stages/probe-builtin/attempts/x/surrender", surrenderedBody("success")),
		"wrong content type":  httptest.NewRequest(http.MethodPost, surrenderPath("run-1", "probe-builtin", 1), nil),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusUnsupportedMediaType && response.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 400/415/404", name, response.Code)
		}
	}
	if len(service.puts) != before {
		t.Fatal("invalid surrenders reached the plane")
	}
}

func TestStageSurrenderMapsPlaneErrorToWriteFailure(t *testing.T) {
	service := &fakeSurrenderService{err: errors.New("disk full")}
	handler := writePlaneHandler(t, nil, AllowAll, WithSurrenderService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, surrenderPath("run-1", "probe-builtin", 1), surrenderedBody("failure")))
	if response.Code < 500 {
		t.Fatalf("status = %d, want a server error for an unclassified plane failure", response.Code)
	}
}

// TestStageSurrenderUnavailableWithoutService: a daemon not configured for
// mode-3 dispatch answers 503, not 404 — the plane exists in the contract,
// the instance does not serve it, exactly like the journal plane's parity
// test.
func TestStageSurrenderUnavailableWithoutService(t *testing.T) {
	handler := writePlaneHandler(t, nil, AllowAll)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, surrenderPath("run-1", "probe-builtin", 1), surrenderedBody("success")))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

// TestPodPrincipalIsConfinedToItsOwnSurrender proves both halves of pod
// containment on the surrender plane, mirroring the journal plane's test:
// RequireRoles admits a pod principal onto the surrender route, and the
// handler binds it to its own run.
func TestPodPrincipalIsConfinedToItsOwnSurrender(t *testing.T) {
	service := &fakeSurrenderService{}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, authenticator, RequireRoles(), WithSurrenderService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, surrenderPath("run-1", "probe-builtin", 1), surrenderedBody("success")))
	if response.Code != http.StatusOK {
		t.Fatalf("pod surrender into its own run: status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.puts) != 1 {
		t.Fatalf("surrender service saw %+v", service.puts)
	}

	// Another run's surrender is refused before the plane.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, surrenderPath("run-2", "probe-builtin", 1), surrenderedBody("success")))
	if response.Code != http.StatusForbidden {
		t.Fatalf("pod surrender into another run: status = %d, want 403", response.Code)
	}
	if len(service.puts) != 1 {
		t.Fatal("cross-run pod surrender reached the plane")
	}

	// Off-plane methods stay refused for pods: a GET on the surrender path
	// has no route at all (405).
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, surrenderPath("run-1", "probe-builtin", 1), nil))
	if response.Code != http.StatusMethodNotAllowed && response.Code != http.StatusForbidden {
		t.Errorf("pod GET on surrender path: status = %d, want refusal", response.Code)
	}
}

// TestSurrenderPlanePathMatchesOnlyTheSurrenderRoute pins the structural
// matcher the authorizer keys pod admission on, mirroring the journal
// plane's equivalent test.
func TestSurrenderPlanePathMatchesOnlyTheSurrenderRoute(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/runs/run-1/stages/probe-builtin/attempts/1/surrender":       true,
		"/api/v1/runs/run-1/stages/probe-builtin/attempts/1":                 false,
		"/api/v1/runs/run-1/stages/probe-builtin/attempts//surrender":        false,
		"/api/v1/runs//stages/probe-builtin/attempts/1/surrender":            false,
		"/api/v1/runs/run-1/x/probe-builtin/attempts/1/surrender":            false,
		"/api/v1/runs/run-1/stages/probe-builtin/x/1/surrender":              false,
		"/api/v1/runs/run-1/stages/probe-builtin/attempts/1/surrender/extra": false,
		"/api/v1/runs/run-1/journal/emit":                                    false,
		"/api/v1/claims/acquire":                                             false,
	} {
		if got := surrenderPlanePath(path); got != want {
			t.Errorf("surrenderPlanePath(%q) = %t, want %t", path, got, want)
		}
	}
}
