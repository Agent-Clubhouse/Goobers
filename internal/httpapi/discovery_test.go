package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

func TestAuthenticatedOpenAPIRequiresPrivateCacheRevalidation(t *testing.T) {
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "viewer", Roles: []Role{RoleView}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(authenticator))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.OpenAPIPath, nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("OpenAPI status=%d cache=%q", response.Code, response.Header().Get("Cache-Control"))
	}
	request := httptest.NewRequest(http.MethodGet, apicontract.OpenAPIPath, nil)
	request.Header.Set("If-None-Match", response.Header().Get("ETag"))
	conditional := httptest.NewRecorder()
	handler.ServeHTTP(conditional, request)
	if conditional.Code != http.StatusNotModified || conditional.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("conditional OpenAPI status=%d cache=%q", conditional.Code, conditional.Header().Get("Cache-Control"))
	}
	authenticator.principal = nil
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized && unauthorized.Code != http.StatusForbidden {
		t.Fatalf("unauthorized conditional OpenAPI status=%d", unauthorized.Code)
	}
}

func TestOpenAPIDrivesEventsResumeHeader(t *testing.T) {
	store := feedTestStoreAt(t, t.TempDir())
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithChangeFeedStream(store))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.OpenAPIPath, nil))
	var document struct {
		Paths map[string]map[string]struct {
			OperationID apicontract.RouteID `json:"operationId"`
			Parameters  []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	for path, methods := range document.Paths {
		operation := methods["get"]
		if operation.OperationID != apicontract.RouteEvents {
			continue
		}
		for _, parameter := range operation.Parameters {
			if parameter.In != "header" || parameter.Name != "Last-Event-ID" {
				continue
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set(parameter.Name, "invalid-resume-cursor")
			result := httptest.NewRecorder()
			handler.ServeHTTP(result, request)
			if result.Code != http.StatusBadRequest || !strings.Contains(result.Body.String(), "invalid_cursor") {
				t.Fatalf("contract-derived cursor response=%d %s", result.Code, result.Body)
			}
			return
		}
		t.Fatal("events operation does not declare a resume header")
	}
	t.Fatal("OpenAPI has no events operation")
}

func TestDiscoveryBootstrapsTheServingDaemonContract(t *testing.T) {
	reader := &fakeReader{err: errors.New("journal unavailable")}
	identity := DiscoveryIdentity{
		DaemonInstanceID: "instance-1",
		DaemonBootID:     "b7fc610d-3c79-4fd4-9bb3-da569b6d14d5",
		Build:            readservice.BuildMetadata{Version: "v1.2.3", Commit: "abc1234"},
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger(), WithDiscoveryIdentity(identity))
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
		discovery.DaemonProtocolVersion != apicontract.DaemonProtocolVersion ||
		discovery.DaemonInstanceID != identity.DaemonInstanceID ||
		discovery.DaemonBootID != identity.DaemonBootID ||
		discovery.PreferredAPIVersion != apicontract.PreferredAPIVersion ||
		len(discovery.OpenAPISHA256) != 64 {
		t.Fatalf("discovery = %+v", discovery)
	}
	if reader.called != 0 {
		t.Fatalf("discovery called the runtime reader %d times", reader.called)
	}
	api := discovery.APIs[apicontract.PreferredAPIVersion]
	if api.OpenAPI.Href != apicontract.OpenAPIPath ||
		api.Capabilities.Href != apicontract.CapabilitiesPath ||
		api.Capabilities.ETag == "" ||
		discovery.Links.Instance.Href != apicontract.InstancePath {
		t.Fatalf("discovery links = %+v", discovery)
	}

	openAPIResponse := httptest.NewRecorder()
	handler.ServeHTTP(openAPIResponse, httptest.NewRequest(http.MethodGet, api.OpenAPI.Href, nil))
	if openAPIResponse.Code != http.StatusOK {
		t.Fatalf("OpenAPI status = %d, body = %s", openAPIResponse.Code, openAPIResponse.Body)
	}
	if contentType := openAPIResponse.Header().Get("Content-Type"); contentType != "application/vnd.oai.openapi+json;version=3.1" {
		t.Fatalf("OpenAPI Content-Type = %q", contentType)
	}
	if etag := strings.Trim(openAPIResponse.Header().Get("ETag"), `"`); etag != discovery.OpenAPISHA256 {
		t.Fatalf("OpenAPI ETag = %q, discovery digest = %q", etag, discovery.OpenAPISHA256)
	}

	conditional := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, api.OpenAPI.Href, nil)
	request.Header.Set("If-None-Match", "W/"+openAPIResponse.Header().Get("ETag"))
	handler.ServeHTTP(conditional, request)
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Fatalf("conditional OpenAPI = %d body=%q", conditional.Code, conditional.Body.String())
	}

	compressed := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, api.OpenAPI.Href, nil)
	request.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(compressed, request)
	if compressed.Code != http.StatusOK || compressed.Header().Get("Content-Encoding") != "gzip" ||
		!strings.HasPrefix(compressed.Header().Get("ETag"), `W/"`) {
		t.Fatalf("compressed OpenAPI = %d headers=%v", compressed.Code, compressed.Header())
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var decompressed bytes.Buffer
	if _, err := decompressed.ReadFrom(gzipReader); err != nil {
		t.Fatal(err)
	}
	if err := gzipReader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decompressed.Bytes(), openAPIResponse.Body.Bytes()) {
		t.Fatal("gzip OpenAPI bytes do not decode to the canonical identity representation")
	}

	notCompressed := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, api.OpenAPI.Href, nil)
	request.Header.Set("Accept-Encoding", "gzip;q=0, *;q=1")
	handler.ServeHTTP(notCompressed, request)
	if notCompressed.Header().Get("Content-Encoding") != "" ||
		notCompressed.Header().Get("ETag") != openAPIResponse.Header().Get("ETag") {
		t.Fatalf("identity OpenAPI headers = %v", notCompressed.Header())
	}
}

func TestDiscoveryRejectsMalformedConfiguredIdentity(t *testing.T) {
	for _, identity := range []DiscoveryIdentity{
		{},
		{DaemonInstanceID: "instance\nsecret"},
		{DaemonInstanceID: strings.Repeat("x", 129)},
		{DaemonInstanceID: "instance-1", DaemonBootID: "not-a-uuid"},
		{DaemonInstanceID: "instance-1", Build: readservice.BuildMetadata{Version: "v1\nsecret"}},
	} {
		if _, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithDiscoveryIdentity(identity)); err == nil {
			t.Fatalf("NewHandler accepted malformed discovery identity: %+v", identity)
		}
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
	if trigger.Available || trigger.Code != "service_unconfigured" ||
		trigger.Reason == "" || trigger.RequiredRole != string(RoleOperate) {
		t.Fatalf("trigger capability = %+v", trigger)
	}
	health := capabilityByID(t, document, apicontract.RouteHealth)
	if !health.Available || !health.RecoverySafe || !health.Remote {
		t.Fatalf("health capability = %+v", health)
	}
	runs := capabilityByID(t, document, apicontract.RouteRuns)
	if runs.Available || !runs.Remote || runs.Code != "recovery_in_progress" ||
		!strings.Contains(runs.Reason, "crash recovery") {
		t.Fatalf("runs capability = %+v", runs)
	}
	if document.DaemonProtocolVersion != apicontract.DaemonProtocolVersion ||
		document.APIVersion != apicontract.PreferredAPIVersion ||
		document.OpenAPISHA256 == "" {
		t.Fatalf("capability protocol = %+v", document)
	}
	if len(document.Routes) != len(apicontract.V1Routes()) {
		t.Fatalf("capability routes = %d, contract routes = %d", len(document.Routes), len(apicontract.V1Routes()))
	}

	recoveringETag := response.Header().Get("ETag")
	ready = true
	readyResponse := httptest.NewRecorder()
	handler.ServeHTTP(readyResponse, httptest.NewRequest(http.MethodGet, apicontract.CapabilitiesPath, nil))
	if readyResponse.Code != http.StatusOK {
		t.Fatalf("ready status = %d body=%s", readyResponse.Code, readyResponse.Body)
	}
	if readyResponse.Header().Get("ETag") == recoveringETag {
		t.Fatal("capability ETag did not change when recovery availability changed")
	}

	conditional := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, apicontract.CapabilitiesPath, nil)
	request.Header.Set("If-None-Match", readyResponse.Header().Get("ETag"))
	handler.ServeHTTP(conditional, request)
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Fatalf("conditional capabilities = %d body=%q", conditional.Code, conditional.Body.String())
	}
}

func TestDiscoveryBootIDChangesAcrossDaemonHandlers(t *testing.T) {
	bootIDs := make([]string, 0, 2)
	for range 2 {
		handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.DiscoveryPath, nil))
		var document apicontract.DiscoveryDocument
		if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
			t.Fatal(err)
		}
		bootIDs = append(bootIDs, document.DaemonBootID)
	}
	if bootIDs[0] == bootIDs[1] {
		t.Fatalf("daemon boot ID was reused across handlers: %q", bootIDs[0])
	}
}

func TestOpenAPIDrivesInitialRemoteRunsInvocation(t *testing.T) {
	reader := &fakeReader{runs: readservice.RunList{Runs: []readservice.RunSummary{{ID: "run-1"}}}}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	openAPIResponse := httptest.NewRecorder()
	handler.ServeHTTP(openAPIResponse, httptest.NewRequest(http.MethodGet, apicontract.OpenAPIPath, nil))
	var document struct {
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Paths map[string]map[string]struct {
			OperationID apicontract.RouteID `json:"operationId"`
			Parameters  []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(openAPIResponse.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	if len(document.Servers) != 1 {
		t.Fatalf("servers = %+v", document.Servers)
	}
	target := ""
	for path, methods := range document.Paths {
		operation := methods["get"]
		if operation.OperationID != apicontract.RouteRuns {
			continue
		}
		hasLimit := false
		for _, parameter := range operation.Parameters {
			if parameter.Name == "limit" && parameter.In == "query" {
				hasLimit = true
			}
		}
		if !hasLimit {
			t.Fatal("generated runs client has no limit parameter")
		}
		target = strings.TrimSuffix(document.Servers[0].URL, "/") + path + "?limit=1"
	}
	if target == "" {
		t.Fatal("generated runs client could not find the runs operation")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusOK || reader.options.Limit != 1 {
		t.Fatalf("generated runs invocation = %d options=%+v body=%s", response.Code, reader.options, response.Body)
	}
	var result readservice.RunList
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || result.Runs[0].ID != "run-1" {
		t.Fatalf("generated runs result = %+v", result)
	}
}

func TestHealthAndReadinessIncludeProtocolSummary(t *testing.T) {
	identity := DiscoveryIdentity{
		DaemonInstanceID: "instance-1",
		DaemonBootID:     "b7fc610d-3c79-4fd4-9bb3-da569b6d14d5",
		Build:            readservice.BuildMetadata{Version: "v1.2.3"},
	}
	handler, err := NewHandler(
		&fakeReader{health: readservice.Health{APIVersion: "v1", SchemaVersion: "v1"}},
		AllowAll,
		discardLogger(),
		WithDiscoveryIdentity(identity),
		WithInstanceReadinessService(staticReadinessService{}),
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{apicontract.HealthPath, apicontract.InstanceReadinessPath} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, response.Code, response.Body)
		}
		var document struct {
			Protocol apicontract.ProtocolSummary `json:"protocol"`
		}
		if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
			t.Fatal(err)
		}
		if document.Protocol.DaemonInstanceID != identity.DaemonInstanceID ||
			document.Protocol.DaemonBootID != identity.DaemonBootID ||
			document.Protocol.OpenAPISHA256 == "" ||
			document.Protocol.CapabilitiesETag == "" {
			t.Fatalf("%s protocol = %+v", path, document.Protocol)
		}
	}
}

type staticReadinessService struct{}

func (staticReadinessService) InstanceReadiness(context.Context) (InstanceReadiness, error) {
	return InstanceReadiness{APIVersion: "v1", SchemaVersion: "v1"}, nil
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
