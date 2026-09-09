package claimsclient

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestAnonymousExecutionObserverRequiresLiteralLoopback(t *testing.T) {
	for _, address := range []string{"https://remote.example", "http://localhost", "http://127.0.0.1.attacker.test", "http://user@127.0.0.1", "http://127.0.0.1?q=1", "file:///tmp/daemon"} {
		if _, err := NewExecutionObserver(HTTPConfig{BaseURL: address, RunID: "run"}); err == nil {
			t.Fatalf("anonymous remote observer accepted: %s", address)
		}
	}
	for _, address := range []string{"http://127.0.0.1:1234", "http://[::1]:1234"} {
		if _, err := NewExecutionObserver(HTTPConfig{BaseURL: address, RunID: "run"}); err != nil {
			t.Fatal(err)
		}
		if _, err := NewHTTP(HTTPConfig{BaseURL: address, RunID: "run"}); err == nil {
			t.Fatal("mutation client gained anonymous access")
		}
	}
}

func TestAnonymousExecutionObserverReadsPolicyWithoutFollowingRedirect(t *testing.T) {
	var redirect atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("anonymous observer fabricated a bearer")
		}
		if redirect.Load() {
			http.Redirect(w, r, "http://192.0.2.1/forbidden", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte(`{"claimVisibility":"local","entries":[]}`))
	}))
	defer server.Close()
	observer, err := NewExecutionObserver(HTTPConfig{BaseURL: server.URL, RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if mode, _, err := observer.ExecutionSnapshot(t.Context()); err != nil || mode != "local" {
		t.Fatalf("policy: %s %v", mode, err)
	}
	redirect.Store(true)
	if _, _, err := observer.ExecutionSnapshot(t.Context()); err == nil {
		t.Fatal("redirect accepted as execution policy")
	}
}
