package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type exactItemTriggerService struct{ fakeTriggerService }

func (*exactItemTriggerService) SupportsExactWorkItems() bool { return true }

func TestExactWorkItemCapabilityAndTransportRequireSupportingService(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "exact"}[supported], func(t *testing.T) {
			legacy := &fakeTriggerService{response: TriggerResponse{AcceptanceID: "accepted", State: "accepted"}}
			var service TriggerService = legacy
			exact := &exactItemTriggerService{fakeTriggerService: *legacy}
			if supported {
				service = exact
			}
			handler := writePlaneHandler(t, nil, AllowAll, WithTriggerService(service),
				WithDiscoveryIdentity(DiscoveryIdentity{DaemonInstanceID: "test-exact"}))
			capabilities := httptest.NewRecorder()
			handler.ServeHTTP(capabilities, httptest.NewRequest(http.MethodGet, apicontract.CapabilitiesPath, nil))
			if capabilities.Code != http.StatusOK {
				t.Fatal(capabilities.Code, capabilities.Body)
			}
			var document apicontract.CapabilityDocument
			if err := json.Unmarshal(capabilities.Body.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, route := range document.Routes {
				if route.ID == apicontract.RouteTriggerIngest {
					found = true
					if route.ExactWorkItem != supported {
						t.Fatal("capability does not describe the service", route)
					}
				}
			}
			if !found {
				t.Fatal("missing trigger capability")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, triggerJSONRequest(http.MethodPost, apicontract.TriggerIngestPath,
				`{"workflow":"implementation","gaggle":"own","workItemId":"42","requestId":"exact-delivery"}`))
			if supported {
				if response.Code != http.StatusAccepted || len(exact.requests) != 1 || exact.requests[0].WorkItemID != "42" {
					t.Fatal(response.Code, response.Body, exact.requests)
				}
			} else if response.Code != http.StatusBadRequest || len(legacy.requests) != 0 {
				t.Fatal("legacy service received an exact selection", response.Code, legacy.requests)
			}
		})
	}
}
