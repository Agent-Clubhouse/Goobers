package httpapi

import (
	"context"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
	"net/http"
	"net/http/httptest"
	"testing"
)

type attributionReaderFake struct {
	fakeReader
	called bool
	gaggle string
}

func (f *attributionReaderFake) RunAttribution(_ context.Context, run, gaggle string) (readservice.RunAttributionResult, error) {
	f.called = true
	f.gaggle = gaggle
	return readservice.RunAttributionResult{RunID: run, Gaggle: gaggle, Availability: "absent"}, nil
}
func TestRunAttributionAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal Principal
		want      int
	}{
		{"viewer", Principal{Subject: "viewer", Roles: []Role{RoleView}}, http.StatusOK},
		{"unmapped", Principal{Subject: "unmapped"}, http.StatusForbidden},
		{"pod", Principal{Subject: "own-run", Issuer: PodPrincipalIssuer}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &attributionReaderFake{}
			handler, err := NewHandler(reader, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: &tc.principal}))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.RunsPath+"/own-run/attribution?gaggle=core", nil))
			if response.Code != tc.want {
				t.Fatalf("%d: %s", response.Code, response.Body)
			}
			if reader.called != (tc.want == http.StatusOK) {
				t.Fatal("reader authorization bypass")
			}
		})
	}
}
func TestRunAttributionRequiresScopeAndSupportsOldReaders(t *testing.T) {
	for _, query := range []string{"", "?gaggle=core&gaggle=foreign"} {
		r := &attributionReaderFake{}
		h, err := NewHandler(r, AllowAll, discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, apicontract.RunsPath+"/run/attribution"+query, nil))
		if w.Code != http.StatusBadRequest || r.called {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	h, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, apicontract.RunsPath+"/run/attribution?gaggle=core", nil))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("%d", w.Code)
	}
}
