package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type fakeConfigAuthoringReader struct{}

func (fakeConfigAuthoringReader) Sources(context.Context) (apicontract.ConfigSourcePage, error) {
	return apicontract.ConfigSourcePage{
		APIVersion:    "v1",
		SchemaVersion: apicontract.AuthoringSchemaVersion,
		Items: []apicontract.ConfigSourceDescriptor{{
			ID: "source:local",
		}},
	}, nil
}

func (fakeConfigAuthoringReader) Documents(context.Context, string) (apicontract.ConfigDocumentPage, error) {
	return apicontract.ConfigDocumentPage{
		APIVersion:    "v1",
		SchemaVersion: apicontract.AuthoringSchemaVersion,
		SourceID:      "source:local",
	}, nil
}

func (fakeConfigAuthoringReader) Document(context.Context, string, string) (apicontract.ConfigDocument, error) {
	return apicontract.ConfigDocument{
		APIVersion:    "v1",
		SchemaVersion: apicontract.AuthoringSchemaVersion,
		SourceID:      "source:local",
	}, nil
}

func TestConfigAuthoringReadRoutesRequireViewRole(t *testing.T) {
	authenticator := &fakeAuthenticator{}
	handler, err := NewHandler(
		&fakeReader{},
		AllowAll,
		discardLogger(),
		WithAuthenticator(authenticator),
		WithConfigAuthoringReader(fakeConfigAuthoringReader{}),
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		principal *Principal
		status    int
	}{
		{name: "unauthenticated", status: http.StatusUnauthorized},
		{name: "unauthorized", principal: &Principal{Subject: "user"}, status: http.StatusForbidden},
		{name: "viewer", principal: &Principal{Subject: "viewer", Roles: []Role{RoleView}}, status: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator.principal = test.principal
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.ConfigSourcesPath, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body, test.status)
			}
		})
	}
}

func TestConfigAuthoringReadRoutesAreMountedFromContract(t *testing.T) {
	handler, err := NewHandler(
		&fakeReader{},
		RequireRoles(),
		discardLogger(),
		WithAuthenticator(&fakeAuthenticator{
			principal: &Principal{Subject: "viewer", Roles: []Role{RoleView}},
		}),
		WithConfigAuthoringReader(fakeConfigAuthoringReader{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		apicontract.ConfigSourcesPath,
		"/api/v1/config/sources/source:local/documents",
		"/api/v1/config/sources/source:local/document?path=manifest.yaml",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body)
		}
	}
}

func TestConfigAuthoringReadRoutesAreDiscoverableWhenConfigured(t *testing.T) {
	handler, err := NewHandler(
		&fakeReader{},
		RequireRoles(),
		discardLogger(),
		WithAuthenticator(&fakeAuthenticator{
			principal: &Principal{Subject: "viewer", Roles: []Role{RoleView}},
		}),
		WithDiscoveryIdentity(DiscoveryIdentity{DaemonInstanceID: "instance-1"}),
		WithConfigAuthoringReader(fakeConfigAuthoringReader{}),
	)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.CapabilitiesPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d, body = %s", response.Code, response.Body)
	}
	var capabilities apicontract.CapabilityDocument
	if err := json.NewDecoder(response.Body).Decode(&capabilities); err != nil {
		t.Fatal(err)
	}
	if got, want := len(capabilities.Routes), len(apicontract.V1Routes())+3; got != want {
		t.Fatalf("capability routes = %d, want %d", got, want)
	}

	for _, route := range apicontract.V1ConfigAuthoringRoutes() {
		if route.Method != http.MethodGet {
			continue
		}
		capability := capabilityByID(t, capabilities, route.ID)
		if !capability.Available || capability.RequiredRole != string(RoleView) {
			t.Fatalf("%s capability = %+v", route.ID, capability)
		}
		path, _ := discoveryOperation(t, handler, route.ID)
		if path != route.Path {
			t.Fatalf("%s OpenAPI path = %q, want %q", route.ID, path, route.Path)
		}
	}
}
