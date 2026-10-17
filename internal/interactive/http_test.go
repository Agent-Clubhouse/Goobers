package interactive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func apiRequest(s *Service, method, path, owner, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Test-Owner", owner)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	return recorder
}
func TestHTTPAuthorizationReplayAndValidation(t *testing.T) {
	opts := options(t, newExecutor())
	opts.Principal = func(r *http.Request) (string, error) { return r.Header.Get("X-Test-Owner"), nil }
	s := openTest(t, opts)
	req := `{"commandId":"create-stable","surfaceId":"work","title":"Review"}`
	created := apiRequest(s, "POST", apiPrefix+"/sessions", "alice", req)
	if created.Code != 201 {
		t.Fatal(created.Code, created.Body.String())
	}
	var session Session
	if err := json.Unmarshal(created.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	replay := apiRequest(s, "POST", apiPrefix+"/sessions", "alice", req)
	var same Session
	json.Unmarshal(replay.Body.Bytes(), &same)
	if same.ID != session.ID {
		t.Fatal("create replay duplicated session")
	}
	if changed := apiRequest(s, "POST", apiPrefix+"/sessions", "alice", strings.Replace(req, "Review", "Other", 1)); changed.Code != 409 {
		t.Fatal("changed create was not rejected")
	}
	for _, suffix := range []string{"", "/events", "/changes"} {
		if read := apiRequest(s, "GET", apiPrefix+"/sessions/"+session.ID+suffix, "bob", ""); read.Code != 404 {
			t.Fatalf("cross-owner %s returned %d", suffix, read.Code)
		}
	}
	if read := apiRequest(s, "GET", apiPrefix+"/sessions", "", ""); read.Code != 401 {
		t.Fatal("missing identity accepted")
	}
	if read := apiRequest(s, "GET", apiPrefix+"/sessions/"+session.ID+"/events?after=999", "alice", ""); read.Code != 409 {
		t.Fatal("future replay cursor accepted")
	}
	if bad := apiRequest(s, "POST", apiPrefix+"/sessions", "alice", `{"commandId":"malformed","surfaceId":"work","owner":"bob"}`); bad.Code != 400 {
		t.Fatal("client identity field accepted")
	}
	request := httptest.NewRequest("POST", apiPrefix+"/sessions", strings.NewReader(req))
	request.Header.Set("X-Test-Owner", "alice")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
}

type streamRecorder struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	flushed chan struct{}
}

func (r *streamRecorder) Header() http.Header { return r.header }
func (r *streamRecorder) WriteHeader(int)     {}
func (r *streamRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}
func (r *streamRecorder) Flush() {
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}
func (r *streamRecorder) text() string { r.mu.Lock(); defer r.mu.Unlock(); return r.body.String() }
func TestSSEReplaysCursorAndRechecksPrincipalWithoutRestartingWork(t *testing.T) {
	var revoked atomic.Bool
	opts := options(t, newExecutor())
	opts.Principal = func(*http.Request) (string, error) {
		if revoked.Load() {
			return "", errors.New("revoked")
		}
		return "alice", nil
	}
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", apiPrefix+"/sessions/"+session.ID+"/events", nil).WithContext(ctx)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Last-Event-ID", "0")
	recorder := &streamRecorder{header: http.Header{}, flushed: make(chan struct{}, 2)}
	done := make(chan struct{})
	go func() { defer close(done); s.Handler().ServeHTTP(recorder, request) }()
	select {
	case <-recorder.flushed:
	case <-time.After(time.Second):
		t.Fatal("stream did not flush replay")
	}
	if text := recorder.text(); !strings.Contains(text, "id: 1\nevent: interactive") || !strings.Contains(text, "session.created") {
		t.Fatal("stream omitted durable replay", text)
	}
	revoked.Store(true)
	createTest(t, s, "alice")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream retained revoked principal")
	}
	if len(s.active) != 0 {
		t.Fatal("SSE attached execution compute")
	}
	events, err := s.Events("alice", session.ID, 1)
	if err != nil || len(events) != 0 {
		t.Fatal("cursor replay repeated already-seen events")
	}
}
