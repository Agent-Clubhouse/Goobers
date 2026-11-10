package decider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTokenSourceSuppliesBearerPerRequest(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(goodBody))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Model: "m", Backoff: time.Millisecond,
		TokenSource: func(context.Context) (string, error) { return "entra-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Decide(context.Background(), Request{State: map[string]any{"x": 1}, Questions: questions()}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(auth, "entra-token") {
		t.Errorf("token source not used: %q", auth)
	}
}

func TestTokenSourceFailureIsHiddenAndNotRetried(t *testing.T) {
	var calls int
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", Model: "m", Backoff: time.Millisecond,
		TokenSource: func(context.Context) (string, error) { calls++; return "", errors.New("secret-detail") }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Decide(context.Background(), Request{State: map[string]any{"x": 1}, Questions: questions()})
	if err == nil || strings.Contains(err.Error(), "secret-detail") {
		t.Fatalf("want sanitized error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("token failure retried %d times", calls)
	}
}

func TestExactlyOneCredentialRequired(t *testing.T) {
	ts := func(context.Context) (string, error) { return "t", nil }
	for _, cfg := range []Config{
		{BaseURL: "https://x", Model: "m"},
		{BaseURL: "https://x", Model: "m", APIKey: "k", TokenSource: ts},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("expected error for %+v", cfg)
		}
	}
}
