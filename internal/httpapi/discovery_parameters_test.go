package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/stateclient"
)

type discoveryParameter struct {
	Name        string `json:"name"`
	In          string `json:"in"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

func discoveryOperation(t *testing.T, handler http.Handler, id apicontract.RouteID) (string, []discoveryParameter) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.OpenAPIPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("OpenAPI status=%d body=%s", response.Code, response.Body)
	}
	var document struct {
		Paths map[string]map[string]struct {
			ID         apicontract.RouteID  `json:"operationId"`
			Parameters []discoveryParameter `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	for path, methods := range document.Paths {
		for _, operation := range methods {
			if operation.ID == id {
				return path, operation.Parameters
			}
		}
	}
	t.Fatalf("operation %s is missing", id)
	return "", nil
}

func TestOpenAPIDrivesWorkItemAndDefectQueries(t *testing.T) {
	reader := &fakeReader{}
	defects := &fakeDefectService{}
	handler, err := NewHandler(reader, AllowAll, discardLogger(),
		WithDiscoveryIdentity(DiscoveryIdentity{DaemonInstanceID: "instance-1"}),
		WithTelemetryReadAvailability(true), WithTelemetryDefectAggregateService(defects))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		id       apicontract.RouteID
		values   map[string]string
		required []string
	}{
		{apicontract.RouteWorkItems, map[string]string{"provider": "github", "kind": "issue", "limit": "1"}, nil},
		{apicontract.RouteWorkItemDetail, map[string]string{"provider": "github", "kind": "issue", "id": "7", "repository": "owner/repo"}, []string{"repository"}},
		{apicontract.RouteTelemetryDefectAggregates, map[string]string{"gaggle": "core", "since": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}, []string{"gaggle", "since"}},
	}
	for _, test := range tests {
		t.Run(string(test.id), func(t *testing.T) {
			path, parameters := discoveryOperation(t, handler, test.id)
			query := url.Values{}
			required := map[string]bool{}
			for _, parameter := range parameters {
				value, ok := test.values[parameter.Name]
				if !ok {
					t.Fatalf("unexpected advertised parameter %q", parameter.Name)
				}
				switch parameter.In {
				case "path":
					path = strings.ReplaceAll(path, "{"+parameter.Name+"}", url.PathEscape(value))
				case "query":
					query.Set(parameter.Name, value)
					required[parameter.Name] = parameter.Required
				}
			}
			for _, name := range test.required {
				if !required[name] {
					t.Fatalf("required query parameter %s is missing", name)
				}
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("contract-derived request status=%d body=%s", response.Code, response.Body)
			}
		})
	}
	if reader.workItemsReq.Limit != 1 || reader.workItemsReq.Provider != "github" || reader.workItemsReq.Kind != "issue" {
		t.Fatalf("work-item query did not reach the reader: %+v", reader.workItemsReq)
	}
	if reader.workItem.Repository != "owner/repo" || reader.workItem.ExternalID != "7" {
		t.Fatalf("work-item identity did not reach the reader: %+v", reader.workItem)
	}
	if defects.calls != 1 || defects.request.Gaggle != "core" || defects.request.Since.IsZero() {
		t.Fatalf("defect query did not reach service: calls=%d request=%+v", defects.calls, defects.request)
	}
}

func TestOpenAPIDrivesStateWritePreconditions(t *testing.T) {
	service := &fakeStateService{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithStateService(service),
		WithDiscoveryIdentity(DiscoveryIdentity{DaemonInstanceID: "instance-1"}))
	if err != nil {
		t.Fatal(err)
	}
	path, parameters := discoveryOperation(t, handler, apicontract.RouteGaggleStatePut)
	path = strings.ReplaceAll(path, "{gaggle}", "core")
	path = strings.ReplaceAll(path, "{key}", stateclient.KeyBlockedRecords)
	headers := map[string]bool{}
	for _, parameter := range parameters {
		if parameter.In == "header" {
			headers[parameter.Name] = true
			if parameter.Required || !strings.Contains(parameter.Description, "Exactly one") {
				t.Fatalf("incorrect mutually-exclusive precondition: %+v", parameter)
			}
		}
	}
	for _, name := range []string{"If-Match", "If-None-Match"} {
		if !headers[name] {
			t.Fatalf("missing state-write precondition %s", name)
		}
	}
	for _, test := range []struct {
		headers map[string]string
		status  int
	}{
		{nil, http.StatusPreconditionRequired},
		{map[string]string{"If-None-Match": "*"}, http.StatusNoContent},
		{map[string]string{"If-Match": `"existing"`}, http.StatusNoContent},
		{map[string]string{"If-Match": `"existing"`, "If-None-Match": "*"}, http.StatusBadRequest},
	} {
		request := httptest.NewRequest(http.MethodPut, path, strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		for name, value := range test.headers {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("headers=%v status=%d want=%d body=%s", test.headers, response.Code, test.status, response.Body)
		}
	}
	if len(service.puts) != 2 || service.puts[0].IfMatch != "" || service.puts[1].IfMatch != "existing" {
		t.Fatalf("preconditions did not reach service: %+v", service.puts)
	}
}

type baseDiscoveryReader struct{ readservice.Reader }

func TestWorkItemCapabilitiesRequireReaderExtension(t *testing.T) {
	for _, extended := range []bool{false, true} {
		backing := &fakeReader{}
		var reader readservice.Reader = baseDiscoveryReader{backing}
		if extended {
			reader = backing
		}
		handler, err := NewHandler(reader, AllowAll, discardLogger(), WithTelemetryReadAvailability(true),
			WithDiscoveryIdentity(DiscoveryIdentity{DaemonInstanceID: "instance-1"}))
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.CapabilitiesPath, nil))
		var document apicontract.CapabilityDocument
		if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
			t.Fatal(err)
		}
		if !capabilityByID(t, document, apicontract.RouteTelemetryStats).Available {
			t.Fatal("work-item support disabled independent telemetry routes")
		}
		for _, id := range []apicontract.RouteID{apicontract.RouteWorkItems, apicontract.RouteWorkItemDetail} {
			capability := capabilityByID(t, document, id)
			if capability.Available != extended || (!extended && (capability.Code == "" || capability.Reason == "")) {
				t.Fatalf("extended=%t capability=%+v", extended, capability)
			}
			path := strings.NewReplacer("{provider}", "github", "{kind}", "issue", "{id}", "7").Replace(capability.Path)
			if id == apicontract.RouteWorkItemDetail {
				path += "?repository=owner/repo"
			}
			result := httptest.NewRecorder()
			handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, path, nil))
			want := http.StatusServiceUnavailable
			if extended {
				want = http.StatusOK
			}
			if result.Code != want {
				t.Fatalf("extended=%t route=%s status=%d want=%d", extended, id, result.Code, want)
			}
		}
	}
}
