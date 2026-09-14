package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

func TestDiscoveryBootstrapsTheServingDaemonContract(t *testing.T) {
	reader := &fakeReader{health: readservice.Health{
		Build: readservice.BuildMetadata{Version: "v1.2.3", Commit: "abc1234"},
	}}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	discoveryResponse := httptest.NewRecorder()
	handler.ServeHTTP(discoveryResponse, httptest.NewRequest(http.MethodGet, apicontract.DiscoveryPath, nil))
	if discoveryResponse.Code != http.StatusOK {
		t.Fatalf("discovery status = %d, body = %s", discoveryResponse.Code, discoveryResponse.Body)
	}
	var discovery apicontract.DiscoveryDocument
	if err := json.NewDecoder(discoveryResponse.Body).Decode(&discovery); err != nil {
		t.Fatal(err)
	}
	if discovery.Product != "goobers" || discovery.DaemonVersion != "v1.2.3" ||
		discovery.DaemonCommit != "abc1234" || discovery.Authentication != "none" ||
		discovery.OpenAPI != apicontract.OpenAPIPath ||
		discovery.Capabilities != apicontract.CapabilitiesPath || len(discovery.OpenAPISHA256) != 64 {
		t.Fatalf("discovery = %+v", discovery)
	}

	openAPIResponse := httptest.NewRecorder()
	handler.ServeHTTP(openAPIResponse, httptest.NewRequest(http.MethodGet, discovery.OpenAPI, nil))
	if openAPIResponse.Code != http.StatusOK {
		t.Fatalf("OpenAPI status = %d, body = %s", openAPIResponse.Code, openAPIResponse.Body)
	}
	if contentType := openAPIResponse.Header().Get("Content-Type"); contentType != "application/vnd.oai.openapi+json;version=3.1" {
		t.Fatalf("OpenAPI Content-Type = %q", contentType)
	}
	if etag := strings.Trim(openAPIResponse.Header().Get("ETag"), `"`); etag != discovery.OpenAPISHA256 {
		t.Fatalf("OpenAPI ETag = %q, discovery digest = %q", etag, discovery.OpenAPISHA256)
	}
}

func TestCapabilitiesReportDeploymentAvailabilityAndRecovery(t *testing.T) {
	ready := false
	handler, err := NewHandler(
		&fakeReader{},
		AllowAll,
		discardLogger(),
		WithRecoveryGate(func() bool { return ready }),
	)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.CapabilitiesPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var document apicontract.CapabilityDocument
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	discovery := capabilityByID(t, document, apicontract.RouteDiscovery)
	if !discovery.Available || !discovery.RecoverySafe {
		t.Fatalf("discovery capability = %+v", discovery)
	}
	trigger := capabilityByID(t, document, apicontract.RouteTriggerIngest)
	if trigger.Available || trigger.Reason == "" || trigger.RequiredRole != string(RoleOperate) {
		t.Fatalf("trigger capability = %+v", trigger)
	}
	health := capabilityByID(t, document, apicontract.RouteHealth)
	if !health.Available || !health.RecoverySafe {
		t.Fatalf("health capability = %+v", health)
	}
	runs := capabilityByID(t, document, apicontract.RouteRuns)
	if runs.Available || !strings.Contains(runs.Reason, "crash recovery") {
		t.Fatalf("runs capability = %+v", runs)
	}
}

func capabilityByID(t *testing.T, document apicontract.CapabilityDocument, id apicontract.RouteID) apicontract.RouteCapability {
	t.Helper()
	for _, route := range document.Routes {
		if route.ID == id {
			return route
		}
	}
	t.Fatalf("capability %q is missing", id)
	return apicontract.RouteCapability{}
}
