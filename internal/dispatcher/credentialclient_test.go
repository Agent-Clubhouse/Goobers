package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCredentialResolveReturnsMintedValues(t *testing.T) {
	var gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"credentials": []map[string]string{{"capability": "contents:write", "value": "tok-abc"}},
		})
	}))
	defer server.Close()

	client := &CredentialResolveClient{BaseURL: server.URL, Token: "goobers-pod.x"}
	creds, err := client.Resolve(context.Background(), "run-1", "open-pr", []string{"contents:write"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(creds) != 1 || creds[0].Value != "tok-abc" {
		t.Fatalf("creds = %+v", creds)
	}
	if gotAuth != "Bearer goobers-pod.x" {
		t.Fatalf("authorization = %q — the plane authenticates the POD, not a human", gotAuth)
	}
	if !strings.Contains(gotBody, "contents:write") || !strings.Contains(gotBody, "run-1") {
		t.Fatalf("request body must carry run and capabilities, got %q", gotBody)
	}
}

// No declared capabilities must mean NO request at all — a stage that needs
// nothing must not cause the daemon to mint anything.
func TestCredentialResolveSkipsTheCallWhenNothingIsDeclared(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	client := &CredentialResolveClient{BaseURL: server.URL}
	creds, err := client.Resolve(context.Background(), "run-1", "s", nil)
	if err != nil || creds != nil {
		t.Fatalf("Resolve = %v, %v, want nil, nil", creds, err)
	}
	if called {
		t.Fatal("resolve must not call the credential plane when no capability is declared")
	}
}

// A refusal must surface WHY. A scoping refusal and a transport fault reading
// the same is what makes a credential problem expensive to diagnose.
func TestCredentialResolveSurfacesRefusalDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"capability contents:write not declared by stage"}`))
	}))
	defer server.Close()
	client := &CredentialResolveClient{BaseURL: server.URL}
	_, err := client.Resolve(context.Background(), "run-1", "s", []string{"contents:write"})
	if err == nil || !strings.Contains(err.Error(), "not declared by stage") {
		t.Fatalf("err = %v, want the plane's refusal detail carried through", err)
	}
}

// An empty value for a GRANTED capability is a fault, not a no-op: the stage
// would run believing it was credentialed.
func TestCredentialResolveRejectsEmptyValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"credentials": []map[string]string{{"capability": "contents:write", "value": ""}},
		})
	}))
	defer server.Close()
	client := &CredentialResolveClient{BaseURL: server.URL}
	if _, err := client.Resolve(context.Background(), "run-1", "s", []string{"contents:write"}); err == nil {
		t.Fatal("an empty credential value must be an error, not a silent no-op")
	}
}
