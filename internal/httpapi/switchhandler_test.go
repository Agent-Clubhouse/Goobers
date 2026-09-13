package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSwitchHandlerReplacesStartupSurface(t *testing.T) {
	startup := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "starting", http.StatusServiceUnavailable)
	})
	handler, err := NewSwitchHandler(startup, true)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("startup status = %d, want 503", response.Code)
	}

	if err := handler.Set(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	})); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("replacement status = %d, want 204", response.Code)
	}
	if !handlerAuthenticated(handler) {
		t.Fatal("switch handler lost its configured authentication posture")
	}
}

func TestSwitchHandlerRejectsMissingHandlers(t *testing.T) {
	if _, err := NewSwitchHandler(nil, false); err == nil {
		t.Fatal("expected missing initial handler error")
	}
	handler, err := NewSwitchHandler(http.NotFoundHandler(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Set(nil); err == nil {
		t.Fatal("expected missing replacement handler error")
	}
}
