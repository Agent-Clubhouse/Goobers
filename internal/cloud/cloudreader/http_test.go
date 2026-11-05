package cloudreader

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
)

// Through the real HTTP handler, a Reader with no applied generation answers
// 503 read_unavailable on both read-error paths, and a Reader with no claim
// source reports active claims unconfigured: never 200 with empty data.
func TestReaderUnavailableReadsAre503OverHTTP(t *testing.T) {
	t.Chdir(t.TempDir())
	reader, err := New(testConfig(t, &appliedSwitch{}))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(reader, httpapi.AllowAll, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	for path, wantCode := range map[string]string{
		apicontract.RunsPath:         "read_unavailable",     // writeReadError
		apicontract.GagglesPath:      "read_unavailable",     // writeInventoryReadError
		apicontract.ClaimsActivePath: "service_unconfigured", // no claim source composed
	} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			var envelope apicontract.ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatalf("%s: undecodable body: %v", path, err)
			}
			if response.Code != http.StatusServiceUnavailable || envelope.Error.Code != wantCode {
				t.Fatalf("%s = %d %q, want 503 %q", path, response.Code, envelope.Error.Code, wantCode)
			}
		})
	}
}
