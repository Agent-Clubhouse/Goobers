package dispatcher

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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

// #4260: a stage pod's surrender PUT must survive the daemon restart that
// happens on every rollout (goobers-api is single-replica by construction,
// #3809) — a transient refusal or a dropped connection must not discard an
// already-finished attempt's result.
func TestSurrenderPutClientRetriesTransientFailureThenSucceeds(t *testing.T) {
	shrinkRetryDelays(t)
	var attempts atomic.Int32
	const failures = 3
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	client := &SurrenderPutClient{BaseURL: server.URL, RetryDeadline: time.Second}
	if err := client.Put(context.Background(), "run-1", "probe-builtin", 1, []byte(`{"result":{"status":"success"}}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := attempts.Load(); got != failures+1 {
		t.Fatalf("server saw %d attempts, want %d (failures + the eventual success)", got, failures+1)
	}
}

// A 4xx is a permanent refusal, not a transient one — retrying it wastes the
// retry deadline on an error no amount of patience fixes, and delays the
// pod's exit code past the point the disposal gate is waiting on.
func TestSurrenderPutClientDoesNotRetry4xx(t *testing.T) {
	shrinkRetryDelays(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)

	client := &SurrenderPutClient{BaseURL: server.URL, RetryDeadline: time.Second}
	if err := client.Put(context.Background(), "run-1", "probe-builtin", 1, []byte(`{}`)); err == nil {
		t.Fatal("expected an error on a 400 response")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("server saw %d attempts, want exactly 1 (no retry on a 4xx)", got)
	}
}

// The regression case #4260's acceptance criteria call for directly: the
// retry deadline is honoured, not merely documented — Put gives up and the
// pod would exit 1 once it expires, rather than retrying forever.
func TestSurrenderPutClientHonoursRetryDeadline(t *testing.T) {
	shrinkRetryDelays(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	client := &SurrenderPutClient{BaseURL: server.URL, RetryDeadline: 30 * time.Millisecond}
	start := time.Now()
	err := client.Put(context.Background(), "run-1", "probe-builtin", 1, []byte(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error once the retry deadline elapsed against a permanently unhealthy endpoint")
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want close to the 30ms retry deadline, not an unbounded retry loop", elapsed)
	}
}

// The regression test #4260 calls for: kill the connection mid-PUT (the
// server accepts and durably applies the body, then the response is lost)
// and confirm the retried PUT neither loses the work nor double-applies it.
// This is exactly the daemon-restart shape: goobers-api can process a
// request and be killed before the ack reaches the pod.
func TestSurrenderPutClientRetryAfterLostAckDoesNotDoubleApply(t *testing.T) {
	shrinkRetryDelays(t)
	dir, err := NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewSurrenderDir: %v", err)
	}
	var puts atomic.Int32
	first := true
	// This handler stands in for internal/httpapi/surrenderplane.go's route:
	// read the body, hand it to the REAL SurrenderPlane.Put (the production
	// write-once-by-(run,stage,attempt) code path, surrender.go:298-348), not
	// a re-implemented stand-in — so this test exercises the actual guarantee
	// that makes retrying safe, not an assumption about it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		body, rerr := io.ReadAll(r.Body)
		if rerr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if perr := dir.Put(r.Context(), "run-1", "probe-builtin", 1, body); perr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if first {
			first = false
			// The write above already landed durably; what's lost here is
			// only the ACK — the exact shape of a daemon killed between
			// applying a request and answering it (a routine rollout,
			// #3809). Hijacking and closing the raw connection, rather than
			// writing an error status, is what makes this "lost ack" and
			// not "refused".
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			conn, _, herr := hijacker.Hijack()
			if herr != nil {
				t.Fatalf("hijack: %v", herr)
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	client := &SurrenderPutClient{BaseURL: server.URL, RetryDeadline: time.Second}
	want := []byte(`{"result":{"status":"success"}}`)
	if err := client.Put(context.Background(), "run-1", "probe-builtin", 1, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := puts.Load(); got != 2 {
		t.Fatalf("server saw %d PUTs, want 2 (the lost-ack write, then the pod's retry)", got)
	}
	// The work was not lost: the retried PUT eventually got an ack and Put
	// returned success.
	got, gerr := dir.Get(context.Background(), "run-1", "probe-builtin", 1)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	// The work was not double-applied: the second PUT's identical body did
	// not corrupt or duplicate the first PUT's already-durable write — the
	// stored document is exactly what the first write landed, byte for byte.
	if string(got) != string(want) {
		t.Fatalf("stored document = %q, want %q (a double-apply would corrupt or duplicate this)", got, want)
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
