package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/readservice"
)

// journalreadplane_test.go is the authorization half of decision 005 R1 /
// #3880. Everything here asks one of two questions: may this principal reach
// this route at all, and — having reached it — may it name this run?

type fakeRunJournalService struct {
	phase         journalclient.RunPhaseResponse
	touches       journalclient.ConflictTouchResponse
	work          journalclient.UnpushedWorkResponse
	candidates    journalclient.EscalationCandidatesResponse
	ownership     journalclient.BranchOwnershipResponse
	err           error
	phaseReqs     []journalclient.RunPhaseRequest
	touchReqs     []journalclient.ConflictTouchRequest
	workReqs      []journalclient.UnpushedWorkRequest
	candidateReqs []journalclient.EscalationCandidatesRequest
	ownershipReqs []journalclient.BranchOwnershipRequest
}

func (f *fakeRunJournalService) RunPhase(_ context.Context, req journalclient.RunPhaseRequest) (journalclient.RunPhaseResponse, error) {
	f.phaseReqs = append(f.phaseReqs, req)
	return f.phase, f.err
}

func (f *fakeRunJournalService) ConflictTouches(_ context.Context, req journalclient.ConflictTouchRequest) (journalclient.ConflictTouchResponse, error) {
	f.touchReqs = append(f.touchReqs, req)
	return f.touches, f.err
}

func (f *fakeRunJournalService) UnpushedWork(_ context.Context, req journalclient.UnpushedWorkRequest) (journalclient.UnpushedWorkResponse, error) {
	f.workReqs = append(f.workReqs, req)
	return f.work, f.err
}

func (f *fakeRunJournalService) EscalationCandidates(_ context.Context, req journalclient.EscalationCandidatesRequest) (journalclient.EscalationCandidatesResponse, error) {
	f.candidateReqs = append(f.candidateReqs, req)
	return f.candidates, f.err
}

func (f *fakeRunJournalService) BranchOwnership(_ context.Context, req journalclient.BranchOwnershipRequest) (journalclient.BranchOwnershipResponse, error) {
	f.ownershipReqs = append(f.ownershipReqs, req)
	return f.ownership, f.err
}

func (f *fakeRunJournalService) calls() int {
	return len(f.phaseReqs) + len(f.touchReqs) + len(f.workReqs) + len(f.candidateReqs) + len(f.ownershipReqs)
}

func podHandler(t *testing.T, runID string, reader readservice.Reader, service RunJournalService) http.Handler {
	t.Helper()
	if reader == nil {
		reader = &fakeReader{health: readservice.Health{Ready: true}}
	}
	opts := []HandlerOption{
		WithAuthenticator(&fakeAuthenticator{principal: &Principal{
			Subject: "run:" + runID, Issuer: PodPrincipalIssuer,
		}}),
	}
	if service != nil {
		opts = append(opts, WithRunJournalService(service))
	}
	handler, err := NewHandler(reader, RequireRoles(), discardLogger(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func crossRunPaths() []string {
	return []string{
		apicontract.JournalRunPhasePath,
		apicontract.JournalConflictTouchesPath,
		apicontract.JournalUnpushedWorkPath,
		apicontract.JournalEscalationCandidatesPath,
		apicontract.JournalBranchOwnershipPath,
	}
}

// TestRunJournalReadRoutesAreInTheContract pins the four cross-run routes
// onto the same §7 discipline the other write planes carry.
func TestRunJournalReadRoutesAreInTheContract(t *testing.T) {
	for _, id := range []apicontract.RouteID{
		apicontract.RouteJournalRunPhase,
		apicontract.RouteJournalConflictTouches,
		apicontract.RouteJournalUnpushedWork,
		apicontract.RouteJournalEscalationCandidates,
		apicontract.RouteJournalBranchOwnership,
	} {
		route, ok := apicontract.V1Route(id)
		if !ok {
			t.Fatalf("route %s is not in the V1 contract", id)
		}
		if route.Method != http.MethodPost {
			t.Errorf("route %s method = %s, want POST", id, route.Method)
		}
		if route.Cost != apicontract.CostMutation || route.Budget != apicontract.MutationBudget {
			t.Errorf("route %s cost/budget = %s/%s, want mutation discipline", id, route.Cost, route.Budget)
		}
	}
}

// TestPodPrincipalReachesOnlyItsOwnRunReadRoutes is the authorizer half of
// the boundary: the three same-run GET shapes are admitted, and every
// neighbouring run-scoped read that decision 005 did NOT authorise is not.
func TestPodPrincipalReachesOnlyItsOwnRunReadRoutes(t *testing.T) {
	reader := &fakeReader{
		health:   readservice.Health{Ready: true},
		events:   readservice.EventList{RunID: "run-1"},
		attempts: readservice.AttemptList{RunID: "run-1", Stage: "implement"},
		artifact: readservice.ArtifactContent{Bytes: []byte("{}")},
	}
	handler := podHandler(t, "run-1", reader, nil)

	for _, path := range []string{
		"/api/v1/runs/run-1/events",
		"/api/v1/runs/run-1/stages/implement/attempts",
		"/api/v1/runs/run-1/artifacts/sha256:" + strings.Repeat("a", 64),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Errorf("pod GET %s: status = %d, body = %s", path, response.Code, response.Body)
		}
	}

	// Everything else on the run surface stays human-only. Transcripts in
	// particular: they carry raw goober output, they are not an input to any
	// converted reader, and the conservative reading of R1 admits only what
	// the ruling enumerated.
	for _, path := range []string{
		"/api/v1/runs/run-1",
		"/api/v1/runs/run-1/transcripts/1",
		"/api/v1/runs",
		"/api/v1/health",
		"/api/v1/instance",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusForbidden {
			t.Errorf("pod GET %s: status = %d, want 403", path, response.Code)
		}
	}
	// Reveal is a POST maintenance route; it must refuse a pod as a pod, not
	// merely as a wrong method.
	revealResponse := httptest.NewRecorder()
	handler.ServeHTTP(revealResponse, jsonRequest(http.MethodPost, "/api/v1/runs/run-1/reveal", `{}`))
	if revealResponse.Code != http.StatusForbidden {
		t.Errorf("pod POST /runs/run-1/reveal: status = %d, want 403", revealResponse.Code)
	}

	// The read routes are reads: a pod may not write through their shapes.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/api/v1/runs/run-1/events", nil))
		if response.Code == http.StatusOK {
			t.Errorf("pod %s /runs/run-1/events succeeded; the read routes are GET-only", method)
		}
	}
}

// TestPodPrincipalCannotReadAnotherRunsJournal is the handler half: the
// authorizer admits the SHAPE, so the run in the path must be checked where
// the run is known.
func TestPodPrincipalCannotReadAnotherRunsJournal(t *testing.T) {
	reader := &fakeReader{
		health:   readservice.Health{Ready: true},
		events:   readservice.EventList{RunID: "run-2"},
		attempts: readservice.AttemptList{RunID: "run-2"},
		artifact: readservice.ArtifactContent{Bytes: []byte("{}")},
	}
	handler := podHandler(t, "run-1", reader, nil)

	for _, path := range []string{
		"/api/v1/runs/run-2/events",
		"/api/v1/runs/run-2/stages/implement/attempts",
		"/api/v1/runs/run-2/artifacts/sha256:" + strings.Repeat("b", 64),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusForbidden {
			t.Fatalf("pod GET %s: status = %d, want 403", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), "run_mismatch") {
			t.Errorf("pod GET %s: body = %s, want run_mismatch", path, response.Body)
		}
	}
}

// TestHumanPrincipalIsUnaffectedByRunContainment guards the regression the
// containment check could have introduced: the portal reads every run.
func TestHumanPrincipalIsUnaffectedByRunContainment(t *testing.T) {
	reader := &fakeReader{
		health:   readservice.Health{Ready: true},
		events:   readservice.EventList{RunID: "run-2"},
		attempts: readservice.AttemptList{RunID: "run-2"},
		artifact: readservice.ArtifactContent{Bytes: []byte("{}")},
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/runs/run-2/events",
		"/api/v1/runs/run-2/stages/implement/attempts",
		"/api/v1/runs/run-2/artifacts/sha256:" + strings.Repeat("c", 64),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Errorf("unauthenticated/human GET %s: status = %d, body = %s", path, response.Code, response.Body)
		}
	}
}

// TestCrossRunRoutesContainThePodToItsOwnRun proves the body-named run is
// checked the same way the path-named one is.
func TestCrossRunRoutesContainThePodToItsOwnRun(t *testing.T) {
	service := &fakeRunJournalService{phase: journalclient.RunPhaseResponse{RunID: "run-9", Phase: "failed"}}
	handler := podHandler(t, "run-1", nil, service)
	since := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)

	ok := map[string]string{
		apicontract.JournalRunPhasePath:             `{"runId":"run-1","targetRunId":"run-9","gaggle":"web"}`,
		apicontract.JournalConflictTouchesPath:      `{"runId":"run-1","gaggle":"web","since":"` + since + `"}`,
		apicontract.JournalUnpushedWorkPath:         `{"runId":"run-1","gaggle":"web","since":"` + since + `"}`,
		apicontract.JournalEscalationCandidatesPath: `{"runId":"run-1","gaggle":"web"}`,
		apicontract.JournalBranchOwnershipPath:      `{"runId":"run-1","targetRunId":"run-9","workflow":"implementation","branch":"goobers/implementation/run-9","gaggle":"web"}`,
	}
	for path, body := range ok {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, path, body))
		if response.Code != http.StatusOK {
			t.Errorf("pod POST %s for its own run: status = %d, body = %s", path, response.Code, response.Body)
		}
	}
	if service.calls() != 5 {
		t.Fatalf("service saw %d calls, want 5", service.calls())
	}

	before := service.calls()
	impostor := map[string]string{
		apicontract.JournalRunPhasePath:             `{"runId":"run-2","targetRunId":"run-9","gaggle":"web"}`,
		apicontract.JournalConflictTouchesPath:      `{"runId":"run-2","gaggle":"web","since":"` + since + `"}`,
		apicontract.JournalUnpushedWorkPath:         `{"runId":"run-2","gaggle":"web","since":"` + since + `"}`,
		apicontract.JournalEscalationCandidatesPath: `{"runId":"run-2","gaggle":"web"}`,
		apicontract.JournalBranchOwnershipPath:      `{"runId":"run-2","targetRunId":"run-9","workflow":"implementation","branch":"goobers/implementation/run-9","gaggle":"web"}`,
	}
	for path, body := range impostor {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, path, body))
		if response.Code != http.StatusForbidden {
			t.Errorf("pod POST %s as run-2: status = %d, want 403", path, response.Code)
		}
	}
	if service.calls() != before {
		t.Fatal("a refused cross-run request reached the service")
	}
}

// TestCrossRunRoutesRefuseUnscopedAndUnboundedRequests is the fail-closed
// validation: no gaggle means no answer (an instance-wide walk is exactly
// what R1 declined to expose), and no window means no answer either.
func TestCrossRunRoutesRefuseUnscopedAndUnboundedRequests(t *testing.T) {
	service := &fakeRunJournalService{}
	handler := podHandler(t, "run-1", nil, service)
	since := time.Now().UTC().Format(time.RFC3339Nano)

	for name, request := range map[string]*http.Request{
		"phase without gaggle":       jsonRequest(http.MethodPost, apicontract.JournalRunPhasePath, `{"runId":"run-1","targetRunId":"run-9"}`),
		"phase without target":       jsonRequest(http.MethodPost, apicontract.JournalRunPhasePath, `{"runId":"run-1","gaggle":"web"}`),
		"phase bad target":           jsonRequest(http.MethodPost, apicontract.JournalRunPhasePath, `{"runId":"run-1","targetRunId":"../etc","gaggle":"web"}`),
		"touches without gaggle":     jsonRequest(http.MethodPost, apicontract.JournalConflictTouchesPath, `{"runId":"run-1","since":"`+since+`"}`),
		"touches without since":      jsonRequest(http.MethodPost, apicontract.JournalConflictTouchesPath, `{"runId":"run-1","gaggle":"web"}`),
		"work without gaggle":        jsonRequest(http.MethodPost, apicontract.JournalUnpushedWorkPath, `{"runId":"run-1","since":"`+since+`"}`),
		"work without since":         jsonRequest(http.MethodPost, apicontract.JournalUnpushedWorkPath, `{"runId":"run-1","gaggle":"web"}`),
		"work oversized inline":      jsonRequest(http.MethodPost, apicontract.JournalUnpushedWorkPath, `{"runId":"run-1","gaggle":"web","since":"`+since+`","maxInlineDiffBytes":999999999}`),
		"work negative inline":       jsonRequest(http.MethodPost, apicontract.JournalUnpushedWorkPath, `{"runId":"run-1","gaggle":"web","since":"`+since+`","maxInlineDiffBytes":-1}`),
		"candidates without gaggle":  jsonRequest(http.MethodPost, apicontract.JournalEscalationCandidatesPath, `{"runId":"run-1"}`),
		"ownership without gaggle":   jsonRequest(http.MethodPost, apicontract.JournalBranchOwnershipPath, `{"runId":"run-1","targetRunId":"run-9","workflow":"implementation","branch":"b"}`),
		"ownership without target":   jsonRequest(http.MethodPost, apicontract.JournalBranchOwnershipPath, `{"runId":"run-1","gaggle":"web","workflow":"implementation","branch":"b"}`),
		"ownership without workflow": jsonRequest(http.MethodPost, apicontract.JournalBranchOwnershipPath, `{"runId":"run-1","targetRunId":"run-9","gaggle":"web","branch":"b"}`),
		"ownership without branch":   jsonRequest(http.MethodPost, apicontract.JournalBranchOwnershipPath, `{"runId":"run-1","targetRunId":"run-9","gaggle":"web","workflow":"implementation"}`),
		"unknown field":              jsonRequest(http.MethodPost, apicontract.JournalUnpushedWorkPath, `{"runId":"run-1","gaggle":"web","since":"`+since+`","surprise":true}`),
		"empty body":                 jsonRequest(http.MethodPost, apicontract.JournalRunPhasePath, ``),
		"wrong content type":         httptest.NewRequest(http.MethodPost, apicontract.JournalRunPhasePath, nil),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: status = %d, want 400/415", name, response.Code)
		}
	}
	if service.calls() != 0 {
		t.Fatalf("invalid cross-run requests reached the service: %d", service.calls())
	}
}

// TestUnpushedWorkRouteDropsCallerSuppliedItemIDs is the whole reason the
// unpushed-work route is purpose-built rather than a generic journal read: a
// pod must not be able to name the item whose stranded work it wants. Whatever
// it sends is blanked at the transport, so no service implementation can
// accidentally honour it.
func TestUnpushedWorkRouteDropsCallerSuppliedItemIDs(t *testing.T) {
	service := &fakeRunJournalService{}
	handler := podHandler(t, "run-1", nil, service)
	since := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.JournalUnpushedWorkPath,
		`{"runId":"run-1","gaggle":"web","since":"`+since+`","itemIds":["42","someone-elses-item"]}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.workReqs) != 1 {
		t.Fatalf("service saw %d requests, want 1", len(service.workReqs))
	}
	if service.workReqs[0].ItemIDs != nil {
		t.Fatalf("service saw caller-supplied itemIds %v; the daemon must derive them", service.workReqs[0].ItemIDs)
	}
}

// TestCrossRunRoutesRefuseWithoutAService proves the plane fails closed when
// the daemon did not wire it: an explicit 503, never an empty success that a
// caller would read as "no prior work exists".
func TestCrossRunRoutesRefuseWithoutAService(t *testing.T) {
	handler := podHandler(t, "run-1", nil, nil)
	since := time.Now().UTC().Format(time.RFC3339Nano)
	for _, path := range crossRunPaths() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, path,
			`{"runId":"run-1","targetRunId":"run-9","gaggle":"web","since":"`+since+`"}`))
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s with no service: status = %d, want 503", path, response.Code)
		}
	}
}

// TestConflictTouchesAnswersAnEmptyListNotNull keeps the wire shape stable for
// a client that ranges over the answer.
func TestConflictTouchesAnswersAnEmptyListNotNull(t *testing.T) {
	service := &fakeRunJournalService{}
	handler := podHandler(t, "run-1", nil, service)
	since := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.JournalConflictTouchesPath,
		`{"runId":"run-1","gaggle":"web","since":"`+since+`"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["touches"]) != "[]" {
		t.Fatalf("touches = %s, want []", decoded["touches"])
	}
}
