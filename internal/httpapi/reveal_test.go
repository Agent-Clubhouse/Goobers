package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/readservice"
)

func TestRunRevealCapabilityAndEndpoint(t *testing.T) {
	var revealed string
	handler, err := NewHandler(
		&fakeReader{portalConfig: readservice.PortalConfig{}},
		AllowAll,
		discardLogger(),
		WithRunRevealer(func(_ context.Context, runID string) error {
			revealed = runID
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	configResponse := httptest.NewRecorder()
	handler.ServeHTTP(configResponse, httptest.NewRequest(http.MethodGet, PortalConfigPath, nil))
	var config readservice.PortalConfig
	if err := json.NewDecoder(configResponse.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	if !config.Capabilities.RevealRun {
		t.Fatal("portal config does not advertise run reveal")
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, RunsPath+"/run-1/reveal", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if revealed != "run-1" {
		t.Fatalf("revealed run = %q, want run-1", revealed)
	}
}

func TestRunRevealFailsClosedWhenUnavailable(t *testing.T) {
	handler, err := NewHandler(&fakeReader{portalConfig: readservice.PortalConfig{}}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	configResponse := httptest.NewRecorder()
	handler.ServeHTTP(configResponse, httptest.NewRequest(http.MethodGet, PortalConfigPath, nil))
	var config readservice.PortalConfig
	if err := json.NewDecoder(configResponse.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	if config.Capabilities.RevealRun {
		t.Fatal("portal config advertises unavailable run reveal")
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, RunsPath+"/run-1/reveal", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

func TestRunRevealReportsMissingRunAndLauncherFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
	}{
		{name: "missing", err: fs.ErrNotExist, code: http.StatusNotFound},
		{name: "launcher", err: errors.New("launcher failed"), code: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, err := NewHandler(
				&fakeReader{},
				AllowAll,
				discardLogger(),
				WithRunRevealer(func(context.Context, string) error { return test.err }),
			)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, RunsPath+"/run-1/reveal", nil))
			if response.Code != test.code {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
		})
	}
}
