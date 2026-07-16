package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

type fakeReader struct {
	health readservice.Health
	err    error
	called int
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func (f *fakeReader) Health(context.Context) (readservice.Health, error) {
	f.called++
	return f.health, f.err
}

func TestHealthHandlerUsesSharedReadService(t *testing.T) {
	reader := &fakeReader{health: readservice.Health{
		APIVersion:    readservice.APIVersion,
		SchemaVersion: readservice.SchemaVersion,
		Ready:         true,
		Instance:      readservice.InstanceIdentity{Name: "example"},
	}}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, HealthPath, nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var health readservice.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if reader.called != 1 || !health.Ready || health.Instance.Name != "example" {
		t.Fatalf("reader called %d times, health = %+v", reader.called, health)
	}
}

func TestAPIErrorsUseStructuredEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		reader     *fakeReader
		method     string
		path       string
		authorizer Authorizer
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown route",
			reader:     &fakeReader{},
			method:     http.MethodGet,
			path:       Prefix + "/missing",
			authorizer: AllowAll,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "method",
			reader:     &fakeReader{},
			method:     http.MethodPost,
			path:       HealthPath,
			authorizer: AllowAll,
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
		},
		{
			name:       "authorization",
			reader:     &fakeReader{},
			method:     http.MethodGet,
			path:       HealthPath,
			authorizer: authorizerFunc(func(*http.Request) error { return errors.New("denied") }),
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:       "read error",
			reader:     &fakeReader{err: errors.New("disk failed")},
			method:     http.MethodGet,
			path:       HealthPath,
			authorizer: AllowAll,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "read_error",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler, err := NewHandler(test.reader, test.authorizer, log.New(&logs, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != test.wantCode || envelope.Error.Message == "" {
				t.Fatalf("error = %+v", envelope.Error)
			}
			if test.wantCode == "read_error" && !strings.Contains(logs.String(), "disk failed") {
				t.Fatalf("server log = %q, want underlying read error", logs.String())
			}
		})
	}
}

func TestServerLifecycleAndStartupFailure(t *testing.T) {
	handler, err := NewHandler(&fakeReader{health: readservice.Health{Ready: true}}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer("127.0.0.1:0", handler, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get("http://" + server.Address() + HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-server.Errors(); ok {
		t.Fatal("errors channel should close after graceful shutdown")
	}

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := occupied.Close(); err != nil {
			t.Errorf("close occupied listener: %v", err)
		}
	})
	blocked, err := NewServer(occupied.Addr().String(), handler, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := blocked.Start(); err == nil {
		t.Fatal("expected occupied listener startup to fail")
	}
}

func TestConstructorsRequireDependencies(t *testing.T) {
	if _, err := NewHandler(nil, AllowAll, discardLogger()); err == nil {
		t.Fatal("expected missing reader error")
	}
	if _, err := NewHandler(&fakeReader{}, nil, discardLogger()); err == nil {
		t.Fatal("expected missing authorizer error")
	}
	if _, err := NewHandler(&fakeReader{}, AllowAll, nil); err == nil {
		t.Fatal("expected missing error logger error")
	}
	if _, err := NewServer("", http.NotFoundHandler(), discardLogger()); err == nil {
		t.Fatal("expected missing address error")
	}
	if _, err := NewServer("127.0.0.1:0", nil, discardLogger()); err == nil {
		t.Fatal("expected missing handler error")
	}
	if _, err := NewServer("127.0.0.1:0", http.NotFoundHandler(), nil); err == nil {
		t.Fatal("expected missing error logger error")
	}
}
