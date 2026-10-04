package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSurrenderReadClientBoundsAndRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		seen       bool
	}{
		{"oversize", strings.Repeat("x", MaxSurrenderReadBytes+1), 200, false},
		{"oversize seen", strings.Repeat("x", 129), 200, true},
		{"missing seen", `{}`, 200, true},
		{"wrong seen", `{"seen":"yes"}`, 200, true},
		{"refused", "sensitive-body-must-not-be-logged", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := &SurrenderReadClient{BaseURL: server.URL, TokenSource: func() (string, error) { return "worker", nil }}
			var err error
			if tc.seen {
				_, err = c.Has(context.Background(), "r", "s", 1)
			} else {
				_, err = c.Get(context.Background(), "r", "s", 1)
			}
			if err == nil || strings.Contains(err.Error(), "sensitive-body") {
				t.Fatalf("unsafe response handling: %v", err)
			}
		})
	}
}

func TestSurrenderDirectoryBoundedReads(t *testing.T) {
	plane, err := NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := plane.Put(ctx, "r", "s", 1, []byte("12345")); err != nil {
		t.Fatal(err)
	}
	if got, err := plane.GetBounded(ctx, "r", "s", 1, 4); got != nil || err == nil {
		t.Fatal("oversize file read accepted")
	}
	if got, err := plane.GetBounded(ctx, "r", "s", 1, 5); err != nil || string(got) != "12345" {
		t.Fatalf("read %q %v", got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := plane.GetBounded(canceled, "r", "s", 1, 5); err == nil {
		t.Fatal("canceled read succeeded")
	}
}

func TestSurrenderReadClientRefusesRedirects(t *testing.T) {
	var reached atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-target" {
			reached.Store(true)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c := &SurrenderReadClient{BaseURL: server.URL, TokenSource: func() (string, error) { return "worker-secret", nil }, Client: server.Client()}
	for _, seen := range []bool{false, true} {
		if _, err := c.fetch(context.Background(), "r", "s", 1, seen); err == nil || strings.Contains(err.Error(), "worker-secret") {
			t.Fatalf("redirect error=%v", err)
		}
	}
	if reached.Load() {
		t.Fatal("worker credential followed redirect")
	}
	if c.Client.CheckRedirect != nil {
		t.Fatal("shared caller client mutated")
	}
}
