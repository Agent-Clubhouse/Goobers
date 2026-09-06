package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	// The whole point of the typed refusal (a pod's Retryable classification
	// depends on errors.As finding it, cmd/goobers/dispatchagentic.go's
	// substrateRetryable): a plane REFUSAL must decode to *CredentialResolveRefusal,
	// carrying the status the plane actually answered with.
	var refusal *CredentialResolveRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v (%T), want errors.As to find a *CredentialResolveRefusal", err, err)
	}
	if refusal.Status != http.StatusForbidden {
		t.Fatalf("refusal.Status = %d, want %d", refusal.Status, http.StatusForbidden)
	}
	if !refusal.Deterministic() {
		t.Fatal("a 403 is the plane's judgement on this request; Deterministic() must be true")
	}
}

// A transport fault — the plane never answered at all — must NOT decode to
// *CredentialResolveRefusal: that type is reserved for a response the plane
// actually sent, and a pod's Retryable classification (substrateRetryable)
// depends on the two staying distinguishable so a dial failure keeps the
// pod's default Retryable=true rather than being read as a stable refusal.
func TestCredentialResolveTransportFaultIsNotARefusal(t *testing.T) {
	// A listener that is opened then immediately closed: connecting to it
	// reliably fails to dial, without depending on network access.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	// A short RetryDeadline: a transport fault is now RETRIED (a refused dial
	// is what a restarting single-replica daemon looks like, #3809), so
	// without one this test would sit out the full default deadline to assert
	// something about classification.
	client := &CredentialResolveClient{BaseURL: "http://" + addr, RetryDeadline: 30 * time.Millisecond}
	_, err = client.Resolve(context.Background(), "run-1", "s", []string{"contents:write"})
	if err == nil {
		t.Fatal("Resolve against a closed listener must fail")
	}
	var refusal *CredentialResolveRefusal
	if errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a dial failure to stay untyped, not decode as a plane refusal", err)
	}
}

// Deterministic is the exact split substrateRetryable relies on: a stable
// 4xx plane judgement (capability_undeclared, gate_pin_missing,
// invalid_request) never changes on a fresh pod, but 408/429 ask the client
// to try again, and anything outside 4xx is the plane's own state, not its
// verdict on the request.
func TestCredentialResolveRefusalDeterministic(t *testing.T) {
	cases := []struct {
		name string
		code int
		want bool
	}{
		{"400 invalid_request", http.StatusBadRequest, true},
		{"403 capability_undeclared", http.StatusForbidden, true},
		{"409 gate_pin_missing", http.StatusConflict, true},
		{"408 request timeout stays transport-shaped", http.StatusRequestTimeout, false},
		{"429 too many requests stays transport-shaped", http.StatusTooManyRequests, false},
		{"500 internal error is the plane's own state", http.StatusInternalServerError, false},
		{"503 credentials_unavailable", http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refusal := &CredentialResolveRefusal{Status: tc.code}
			if got := refusal.Deterministic(); got != tc.want {
				t.Fatalf("Deterministic() for status %d = %v, want %v", tc.code, got, tc.want)
			}
		})
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

// TestCredentialResolveRidesOutARestart is #3809's item (3) for the third and
// last unretried plane: a daemon restart is a ROUTINE event (the daemon is
// single-replica by construction, so every upgrade is stop-then-start), and a
// routine event must not fail in-flight work.
//
// Before this, a resolve that landed in the restart window failed outright —
// the plane's own doc noted that recovery came from spending a FRESH POD,
// a whole dispatch cycle to survive something that lasts seconds to minutes.
func TestCredentialResolveRidesOutARestart(t *testing.T) {
	origBase, origMax := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = origBase, origMax })

	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			// What a restarting control plane answers with.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"credentials_unavailable"}`))
			return
		}
		// The body must survive being retried: an *http.Request body is
		// consumed by the first send, so a naive retry posts an empty body
		// and is refused as invalid — a self-inflicted non-retryable failure.
		var decoded struct {
			RunID        string   `json:"runId"`
			Capabilities []string `json:"capabilities"`
		}
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			t.Errorf("attempt %d posted an undecodable body: %v", attempts, err)
		}
		if decoded.RunID != "run-1" || len(decoded.Capabilities) != 1 {
			t.Errorf("attempt %d posted %+v, want the original request replayed intact", attempts, decoded)
		}
		_, _ = w.Write([]byte(`{"credentials":[{"capability":"contents:write","value":"tok"}]}`))
	}))
	t.Cleanup(server.Close)

	client := &CredentialResolveClient{BaseURL: server.URL, RetryDeadline: 5 * time.Second}
	credentials, err := client.Resolve(context.Background(), "run-1", "s", []string{"contents:write"})
	if err != nil {
		t.Fatalf("Resolve did not survive a restarting plane: %v", err)
	}
	if attempts < 3 {
		t.Fatalf("attempts = %d, want the resolve to have been retried", attempts)
	}
	if len(credentials) != 1 || credentials[0].Value != "tok" {
		t.Fatalf("credentials = %+v, want the minted value once the plane recovered", credentials)
	}
}

// A refusal the plane will repeat for every pod of this stage must still fail
// FAST. Retrying a configuration outcome would turn a clear diagnostic into a
// timeout, and spend the retry budget reproducing an answer that cannot change.
func TestCredentialResolveDoesNotRetryARefusal(t *testing.T) {
	for _, status := range []int{
		http.StatusForbidden,  // capability_undeclared
		http.StatusConflict,   // gate_pin_missing
		http.StatusBadRequest, // invalid_request
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"code":"capability_undeclared"}`))
			}))
			t.Cleanup(server.Close)

			client := &CredentialResolveClient{BaseURL: server.URL, RetryDeadline: 5 * time.Second}
			start := time.Now()
			_, err := client.Resolve(context.Background(), "run-1", "s", []string{"contents:write"})
			if err == nil {
				t.Fatal("a plane refusal was not reported as an error")
			}
			var refusal *CredentialResolveRefusal
			if !errors.As(err, &refusal) || refusal.Status != status {
				t.Fatalf("err = %v, want a typed refusal carrying status %d", err, status)
			}
			if attempts != 1 {
				t.Errorf("attempts = %d, want exactly 1 — a repeatable refusal must not be retried", attempts)
			}
			// Guard the intent rather than a wall-clock threshold: one attempt
			// is the assertion above; this only catches a deadline being burnt.
			if elapsed := time.Since(start); elapsed > 4*time.Second {
				t.Errorf("a refusal took %s; it must fail fast, not spend the retry deadline", elapsed)
			}
		})
	}
}
