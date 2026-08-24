package dispatcher

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeSurrenderEndpoint is an in-memory daemon-fronted surrender endpoint
// requiring the pod-scoped bearer, recording each accepted PUT's identity
// and body.
type surrenderPUT struct {
	path  string
	token string
	body  []byte
}

func fakeSurrenderEndpoint(t *testing.T, token string) (*httptest.Server, *[]surrenderPUT) {
	t.Helper()
	var puts []surrenderPUT
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		puts = append(puts, surrenderPUT{path: r.URL.Path, token: token, body: body})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server, &puts
}

func TestSurrenderPutClientPostsIdentityAndBody(t *testing.T) {
	server, puts := fakeSurrenderEndpoint(t, "pod-token")
	client := &SurrenderPutClient{BaseURL: server.URL, Token: "pod-token"}

	if err := client.Put(context.Background(), "run-1", "probe-builtin", 2, []byte(`{"result":{"status":"success"}}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(*puts) != 1 {
		t.Fatalf("endpoint saw %d requests, want 1", len(*puts))
	}
	got := (*puts)[0]
	if want := "/api/v1/runs/run-1/stages/probe-builtin/attempts/2/surrender"; got.path != want {
		t.Fatalf("path = %q, want %q", got.path, want)
	}
	if string(got.body) != `{"result":{"status":"success"}}` {
		t.Fatalf("body = %q", got.body)
	}
}

// The bearer is not optional decoration: a wrong or missing token is refused
// by the endpoint and surfaces as an error, never as silent success — the
// same contract BlobClient's credential test asserts.
func TestSurrenderPutClientRequiresCredential(t *testing.T) {
	server, puts := fakeSurrenderEndpoint(t, "pod-token")
	client := &SurrenderPutClient{BaseURL: server.URL, Token: "wrong"}
	if err := client.Put(context.Background(), "run-1", "probe-builtin", 1, []byte(`{"result":{"status":"success"}}`)); err == nil {
		t.Fatal("unauthenticated Put succeeded")
	}
	if len(*puts) != 0 {
		t.Fatal("endpoint accepted an unauthenticated request")
	}
}

func TestSurrenderPutClientRequiresIdentity(t *testing.T) {
	client := &SurrenderPutClient{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, call := range map[string]func() error{
		"no base URL": func() error { return (&SurrenderPutClient{Token: "t"}).Put(context.Background(), "r", "s", 1, nil) },
		"no run":      func() error { return client.Put(context.Background(), "", "s", 1, nil) },
		"no stage":    func() error { return client.Put(context.Background(), "r", "", 1, nil) },
		"bad attempt": func() error { return client.Put(context.Background(), "r", "s", 0, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("expected a validation error, got none (and no network call should have been attempted)")
			}
		})
	}
}

func TestSurrenderPutClientNon2xxIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"already_surrendered","message":"nope"}}`))
	}))
	t.Cleanup(server.Close)
	client := &SurrenderPutClient{BaseURL: server.URL}
	if err := client.Put(context.Background(), "run-1", "probe-builtin", 1, []byte(`{}`)); err == nil {
		t.Fatal("expected an error on a non-2xx response")
	}
}
