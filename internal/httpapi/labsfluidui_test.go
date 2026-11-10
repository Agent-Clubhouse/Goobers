package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/fluidui"
)

const fluidUIDoc = `{"version":"portal.customization/v1","density":"compact"}`

func newFluidUIHandler(t *testing.T, store *fluidui.Store, auth *fakeAuthenticator) http.Handler {
	t.Helper()
	opts := []HandlerOption{WithLabsFluidUI(store)}
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

func fluidUIRequest(t *testing.T, handler http.Handler, method, path, body string) (int, fluidui.State, string) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var state fluidui.State
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
	return response.Code, state, response.Body.String()
}

func TestLabsFluidUIRoundTripUndoAndReset(t *testing.T) {
	handler := newFluidUIHandler(t, fluidui.NewStore(t.TempDir()), nil)
	path := apicontract.LabsFluidUICustomizationPath

	code, state, _ := fluidUIRequest(t, handler, http.MethodGet, path, "")
	if code != http.StatusOK || state.Customization != nil {
		t.Fatalf("initial GET = %d %+v", code, state)
	}
	if code, _, body := fluidUIRequest(t, handler, http.MethodPut, path, fluidUIDoc); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	code, state, _ = fluidUIRequest(t, handler, http.MethodDelete, path, "{}")
	if code != http.StatusOK || state.Customization != nil || state.UndoDepth != 1 {
		t.Fatalf("DELETE = %d %+v", code, state)
	}
	code, state, _ = fluidUIRequest(t, handler, http.MethodPost, apicontract.LabsFluidUICustomizationUndoPath, "")
	if code != http.StatusOK || state.Customization == nil || state.Customization.Density != fluidui.DensityCompact {
		t.Fatalf("undo = %d %+v", code, state)
	}
}

func TestLabsFluidUIRejectsInvalidDocumentsAndTransport(t *testing.T) {
	handler := newFluidUIHandler(t, fluidui.NewStore(t.TempDir()), nil)
	path := apicontract.LabsFluidUICustomizationPath

	bad := `{"version":"portal.customization/v1","savedViews":[{"name":"x","route":"javascript:alert(1)"}]}`
	code, _, body := fluidUIRequest(t, handler, http.MethodPut, path, bad)
	if code != http.StatusBadRequest || !strings.Contains(body, "savedViews[0].route") {
		t.Fatalf("invalid PUT = %d %s", code, body)
	}

	request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(fluidUIDoc))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("PUT without JSON content type = %d", response.Code)
	}

	code, _, body = fluidUIRequest(t, handler, http.MethodPost, apicontract.LabsFluidUICustomizationUndoPath, "")
	if code != http.StatusConflict {
		t.Fatalf("undo with no history = %d %s", code, body)
	}
}

func TestLabsFluidUIScopesToPrincipalAndAdmitsViewRole(t *testing.T) {
	store := fluidui.NewStore(t.TempDir())
	path := apicontract.LabsFluidUICustomizationPath
	alice := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "alice", Roles: []Role{RoleView}}}
	bob := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "bob", Roles: []Role{RoleView}}}

	if code, _, body := fluidUIRequest(t, newFluidUIHandler(t, store, alice), http.MethodPut, path, fluidUIDoc); code != http.StatusOK {
		t.Fatalf("view-role PUT = %d %s", code, body)
	}
	code, state, _ := fluidUIRequest(t, newFluidUIHandler(t, store, bob), http.MethodGet, path, "")
	if code != http.StatusOK || state.Customization != nil {
		t.Fatalf("bob sees alice's customization: %d %+v", code, state)
	}
	code, state, _ = fluidUIRequest(t, newFluidUIHandler(t, store, alice), http.MethodGet, path, "")
	if code != http.StatusOK || state.Customization == nil {
		t.Fatalf("alice GET = %d %+v", code, state)
	}

	noRole := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "carol"}}
	if code, _, _ := fluidUIRequest(t, newFluidUIHandler(t, store, noRole), http.MethodPut, path, fluidUIDoc); code != http.StatusForbidden {
		t.Fatalf("role-less PUT = %d, want 403", code)
	}
	pod := &fakeAuthenticator{principal: &Principal{Issuer: PodPrincipalIssuer, Subject: "run-1"}}
	if code, _, _ := fluidUIRequest(t, newFluidUIHandler(t, store, pod), http.MethodPut, path, fluidUIDoc); code != http.StatusForbidden {
		t.Fatalf("pod PUT = %d, want 403", code)
	}
}
