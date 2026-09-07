package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

func TestTelemetryCostRouteParsesAndReturnsSharedContract(t *testing.T) {
	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	reader := &fakeReader{costs: readservice.TelemetryCostResult{
		Provider: "github", Scope: "pr", ExternalID: "4398", Since: since, Until: until,
		PullRequests: []readservice.TelemetryCostAggregate{{
			Provider: "github", ExternalKind: "pr", ExternalID: "4398",
			NativeTotals:     []readservice.TelemetryCostAmount{},
			NormalizedTotals: []readservice.TelemetryCostAmount{},
			BillingModels:    []string{}, CostBases: []string{},
			Models: []readservice.TelemetryCostModelAggregate{},
		}},
		Issues: []readservice.TelemetryCostAggregate{},
	}}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	requestURL := apicontract.TelemetryCostsPath +
		"?provider=github&scope=pr&id=4398&since=" + since.Format(time.RFC3339) +
		"&until=" + until.Format(time.RFC3339)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if reader.costReq.Provider != "github" || reader.costReq.Scope != "pr" ||
		reader.costReq.ExternalID != "4398" ||
		!reader.costReq.Since.Equal(since) || !reader.costReq.Until.Equal(until) {
		t.Fatalf("request = %+v", reader.costReq)
	}
	var decoded readservice.TelemetryCostResult
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.PullRequests) != 1 || decoded.PullRequests[0].ExternalID != "4398" {
		t.Fatalf("body = %+v", decoded)
	}
}

func TestTelemetryCostRouteRejectsMalformedQuery(t *testing.T) {
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"?since=yesterday&until=2026-08-02T00:00:00Z",
		"?since=2026-08-01T00:00:00Z&until=tomorrow",
		"?since=2026-08-01T00:00:00Z&until=2026-08-02T00:00:00Z&sort=cost",
		"?since=2026-08-01T00:00:00Z&since=2026-08-01T01:00:00Z&until=2026-08-02T00:00:00Z",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.TelemetryCostsPath+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400; body = %s", query, response.Code, response.Body)
		}
	}
}
