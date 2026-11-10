package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/fluidui"
	"github.com/goobers/goobers/internal/journal"
)

const fluidUITestRunID = "0123456789abcdef0123456789abcdef"

type fluidUIRequestFixture struct {
	store    *fluidui.Store
	requests *fluidui.Requests
	runs     map[string]fluidui.RunState
	triggers *fakeTriggerService
}

func newFluidUIRequestFixture(t *testing.T) *fluidUIRequestFixture {
	t.Helper()
	f := &fluidUIRequestFixture{
		store:    fluidui.NewStore(t.TempDir()),
		runs:     map[string]fluidui.RunState{},
		triggers: &fakeTriggerService{response: TriggerResponse{AcceptanceID: "trigger-" + fluidUITestRunID, State: "accepted"}},
	}
	f.requests = fluidui.NewRequests(f.store, func(runID string) (fluidui.RunState, error) { return f.runs[runID], nil })
	return f
}

func (f *fluidUIRequestFixture) handler(t *testing.T, auth *fakeAuthenticator, withTriggers bool) http.Handler {
	t.Helper()
	opts := []HandlerOption{WithLabsFluidUI(f.store), WithLabsFluidUIRequests(f.requests)}
	if withTriggers {
		opts = append(opts, WithTriggerService(f.triggers))
	}
	authorizer := AllowAll
	if auth != nil {
		opts = append(opts, WithAuthenticator(auth))
		authorizer = RequireRoles()
	}
	handler, err := NewHandler(&fakeReader{}, authorizer, discardLogger(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func fluidUICall(t *testing.T, handler http.Handler, method, path, body string, out any) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if out != nil && response.Code < 300 {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
	return response.Code, response.Body.String()
}

func fluidUIRequestPath(id, action string) string {
	path := apicontract.LabsFluidUIRequestsPath + "/" + id
	if action != "" {
		path += "/" + action
	}
	return path
}

func TestLabsFluidUIRequestLifecycleThroughHandlers(t *testing.T) {
	f := newFluidUIRequestFixture(t)
	alice := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "alice", Roles: []Role{RoleView}}}
	handler := f.handler(t, alice, true)

	var submitted fluidui.Request
	if code, body := fluidUICall(t, handler, http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"make it compact"}`, &submitted); code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", code, body)
	}
	if submitted.Status != fluidui.StatusQueued || submitted.RunID != fluidUITestRunID {
		t.Fatalf("submitted = %+v", submitted)
	}
	if len(f.triggers.requests) != 1 || f.triggers.requests[0].Workflow != fluidui.Workflow || f.triggers.requests[0].Actor != "alice" {
		t.Fatalf("trigger requests = %+v", f.triggers.requests)
	}
	if strings.Contains(f.triggers.requests[0].RequestID, "compact") {
		t.Fatalf("trigger key carries the prompt: %q", f.triggers.requests[0].RequestID)
	}

	var got fluidui.Request
	if code, _ := fluidUICall(t, handler, http.MethodPost, fluidUIRequestPath(submitted.ID, "apply"), "", nil); code != http.StatusConflict {
		t.Fatalf("apply while queued = %d, want 409", code)
	}
	f.runs[fluidUITestRunID] = fluidui.RunState{Phase: journal.PhaseRunning}
	if fluidUICall(t, handler, http.MethodGet, fluidUIRequestPath(submitted.ID, ""), "", &got); got.Status != fluidui.StatusImplementing {
		t.Fatalf("running request = %+v", got)
	}
	result := fluidui.EvaluateCandidate([]byte(`{"version":"portal.customization/v1","density":"compact"}`))
	f.runs[fluidUITestRunID] = fluidui.RunState{Phase: journal.PhaseCompleted, Result: &result}
	var list labsFluidUIRequestList
	if code, _ := fluidUICall(t, handler, http.MethodGet, apicontract.LabsFluidUIRequestsPath, "", &list); code != http.StatusOK || len(list.Requests) != 1 || list.Requests[0].Status != fluidui.StatusReadyToPreview {
		t.Fatalf("list = %d %+v", code, list)
	}
	if state, _ := f.store.Get("oidc:alice"); state.Customization != nil {
		t.Fatalf("a ready candidate was applied without the user: %+v", state)
	}

	var applied labsFluidUIApplication
	if code, body := fluidUICall(t, handler, http.MethodPost, fluidUIRequestPath(submitted.ID, "apply"), "{}", &applied); code != http.StatusOK {
		t.Fatalf("apply = %d %s", code, body)
	}
	if applied.Request.Status != fluidui.StatusApplied || applied.Customization.Customization == nil || applied.Customization.Customization.Density != fluidui.DensityCompact {
		t.Fatalf("applied = %+v", applied)
	}
	if code, body := fluidUICall(t, handler, http.MethodPost, fluidUIRequestPath(submitted.ID, "revert"), "", &applied); code != http.StatusOK || applied.Request.Status != fluidui.StatusReverted || applied.Customization.Customization != nil {
		t.Fatalf("revert = %d %s", code, body)
	}
}

func TestLabsFluidUIRequestsArePrivateAndAuthorized(t *testing.T) {
	f := newFluidUIRequestFixture(t)
	alice := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "alice", Roles: []Role{RoleView}}}
	bob := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "bob", Roles: []Role{RoleView}}}

	var submitted fluidui.Request
	if code, body := fluidUICall(t, f.handler(t, alice, true), http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"hide the activity panel"}`, &submitted); code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", code, body)
	}
	bobHandler := f.handler(t, bob, true)
	for _, action := range []string{"", "retry", "apply", "revert"} {
		method := http.MethodPost
		if action == "" {
			method = http.MethodGet
		}
		if code, body := fluidUICall(t, bobHandler, method, fluidUIRequestPath(submitted.ID, action), "", nil); code != http.StatusNotFound || strings.Contains(body, "activity") {
			t.Fatalf("bob %s %q = %d %s, want 404", method, action, code, body)
		}
	}
	var list labsFluidUIRequestList
	if fluidUICall(t, bobHandler, http.MethodGet, apicontract.LabsFluidUIRequestsPath, "", &list); len(list.Requests) != 0 {
		t.Fatalf("bob lists alice's requests: %+v", list)
	}

	pod := &fakeAuthenticator{principal: &Principal{Issuer: PodPrincipalIssuer, Subject: "run-1"}}
	if code, _ := fluidUICall(t, f.handler(t, pod, true), http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"x"}`, nil); code != http.StatusForbidden {
		t.Fatalf("pod submit = %d, want 403", code)
	}
	noRole := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "carol"}}
	if code, _ := fluidUICall(t, f.handler(t, noRole, true), http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"x"}`, nil); code != http.StatusForbidden {
		t.Fatalf("role-less submit = %d, want 403", code)
	}
}

func TestLabsFluidUIRequestRejectsBadInputAndMissingTriggers(t *testing.T) {
	f := newFluidUIRequestFixture(t)
	handler := f.handler(t, nil, true)
	for name, body := range map[string]string{
		"unknown field": `{"prompt":"x","workflow":"other"}`,
		"empty prompt":  `{"prompt":"   "}`,
		"too short":     `{"prompt":"dark"}`,
		"not json":      `make it blue`,
		"too long":      `{"prompt":"` + strings.Repeat("a", fluidui.MaxPromptBytes+1) + `"}`,
	} {
		if code, _ := fluidUICall(t, handler, http.MethodPost, apicontract.LabsFluidUIRequestsPath, body, nil); code != http.StatusBadRequest {
			t.Fatalf("%s submit = %d, want 400", name, code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, apicontract.LabsFluidUIRequestsPath, strings.NewReader(`{"prompt":"x"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("submit without JSON content type = %d", response.Code)
	}
	if len(f.triggers.requests) != 0 {
		t.Fatalf("rejected submissions reached the trigger plane: %+v", f.triggers.requests)
	}

	if code, _ := fluidUICall(t, f.handler(t, nil, false), http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"make it compact"}`, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("submit without triggers = %d, want 503", code)
	}
}
