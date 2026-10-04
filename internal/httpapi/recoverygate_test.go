package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestRecoveryGateRefusalIsRetryable pins the crash-recovery answer clients
// wait out after a daemon restart (#5897): HTTP 503, the stable "recovering"
// code, and a Retry-After hint. The code is matched literally because a newer
// CLI recognizes an older daemon by it.
func TestRecoveryGateRefusalIsRetryable(t *testing.T) {
	ready := false
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithRecoveryGate(func() bool { return ready }))
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, InstancePath, nil))
	var envelope ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("recovering body %q: %v", response.Body.String(), err)
	}
	if response.Code != http.StatusServiceUnavailable || envelope.Error.Code != "recovering" || CodeRecovering != "recovering" {
		t.Fatalf("recovering answer = %d %+v, want 503 with code %q", response.Code, envelope.Error, "recovering")
	}
	if got := response.Header().Get(HeaderRetryAfterSeconds); got != strconv.Itoa(NotReadyRetryAfterSeconds) {
		t.Fatalf("recovering Retry-After = %q, want %d", got, NotReadyRetryAfterSeconds)
	}

	ready = true
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, InstancePath, nil))
	if response.Code == http.StatusServiceUnavailable || response.Header().Get(HeaderRetryAfterSeconds) != "" {
		t.Fatalf("ready answer = %d Retry-After=%q, want the route served without a retry hint",
			response.Code, response.Header().Get(HeaderRetryAfterSeconds))
	}
}
