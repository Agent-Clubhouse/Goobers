package stateclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// planeServer is a minimal, faithful implementation of the scheduler-state
// route: one in-memory value per key, guarded by a mutex, with the same
// If-Match / If-None-Match contract the daemon serves. It exists so the client
// half can be tested against the wire contract rather than against the
// daemon's own service (which internal/httpapi tests in its own right).
type planeServer struct {
	t *testing.T

	mu     sync.Mutex
	values map[string][]byte

	gaggle   string
	token    string
	requests atomic.Int32
	// interpose runs before each write is applied, while the server's lock is
	// NOT held, so a test can inject a competing write into the exact window a
	// compare-and-swap is meant to detect.
	interpose func()
}

func newPlaneServer(t *testing.T) (*planeServer, *httptest.Server) {
	t.Helper()
	plane := &planeServer{t: t, values: map[string][]byte{}, gaggle: "goobers", token: "state-token"}
	server := httptest.NewServer(plane)
	t.Cleanup(server.Close)
	return plane, server
}

func (p *planeServer) set(key string, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.values[key] = data
}

func (p *planeServer) get(key string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, ok := p.values[key]
	return data, ok
}

func (p *planeServer) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	p.requests.Add(1)
	if request.Header.Get("Authorization") != "Bearer "+p.token {
		http.Error(w, `{"error":{"code":"unauthorized"}}`, http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.EscapedPath(), "/api/v1/gaggles/"), "/")
	if len(parts) != 3 || parts[1] != "state" {
		http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
		return
	}
	if parts[0] != p.gaggle {
		http.Error(w, `{"error":{"code":"gaggle_mismatch","message":"not your gaggle"}}`, http.StatusForbidden)
		return
	}
	key := parts[2]
	if !ValidKey(key) {
		http.Error(w, `{"error":{"code":"invalid_state_key"}}`, http.StatusBadRequest)
		return
	}
	switch request.Method {
	case http.MethodGet:
		data, ok := p.get(key)
		if !ok {
			http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"`+ETagFor(data)+`"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	case http.MethodPut:
		body, err := io.ReadAll(request.Body)
		if err != nil {
			p.t.Errorf("read body: %v", err)
		}
		ifMatch := strings.Trim(request.Header.Get("If-Match"), `"`)
		ifNoneMatch := request.Header.Get("If-None-Match")
		if ifMatch == "" && ifNoneMatch == "" {
			http.Error(w, `{"error":{"code":"precondition_required"}}`, http.StatusPreconditionRequired)
			return
		}
		if p.interpose != nil {
			p.interpose()
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		current, exists := p.values[key]
		switch {
		case ifNoneMatch != "" && exists:
			http.Error(w, `{"error":{"code":"precondition_failed"}}`, http.StatusPreconditionFailed)
			return
		case ifMatch != "" && (!exists || ETagFor(current) != ifMatch):
			http.Error(w, `{"error":{"code":"precondition_failed"}}`, http.StatusPreconditionFailed)
			return
		}
		p.values[key] = body
		w.Header().Set("ETag", `"`+ETagFor(body)+`"`)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, `{"error":{"code":"method_not_allowed"}}`, http.StatusMethodNotAllowed)
	}
}

func planeClient(t *testing.T, server *httptest.Server) *HTTP {
	t.Helper()
	store, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "state-token", Gaggle: "goobers"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestHTTPStoreRoundTrip pins the client onto the route's contract: a 404 is
// the absent key's zero value, a create carries If-None-Match, and a replace
// carries the ETag that was read.
func TestHTTPStoreRoundTrip(t *testing.T) {
	plane, server := newPlaneServer(t)
	store := planeClient(t, server)
	ctx := t.Context()

	value, err := store.Get(ctx, KeyBlockedRecords)
	if err != nil {
		t.Fatal(err)
	}
	if value.Exists() {
		t.Fatalf("absent key read as %+v, want the zero value", value)
	}

	created, err := store.Put(ctx, KeyBlockedRecords, []byte(`{"a":1}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if created.ETag != ETagFor([]byte(`{"a":1}`)) {
		t.Fatalf("etag = %q", created.ETag)
	}
	stored, ok := plane.get(KeyBlockedRecords)
	if !ok || string(stored) != `{"a":1}` {
		t.Fatalf("plane holds %q", stored)
	}

	read, err := store.Get(ctx, KeyBlockedRecords)
	if err != nil {
		t.Fatal(err)
	}
	if string(read.Data) != `{"a":1}` || read.ETag != created.ETag {
		t.Fatalf("read = %+v", read)
	}

	if _, err := store.Put(ctx, KeyBlockedRecords, []byte(`{"b":2}`), ""); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("create over an existing key: err = %v, want ErrPreconditionFailed", err)
	}
	if _, err := store.Put(ctx, KeyBlockedRecords, []byte(`{"c":3}`), ETagFor([]byte("stale"))); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale replace: err = %v, want ErrPreconditionFailed", err)
	}
	if _, err := store.Put(ctx, KeyBlockedRecords, []byte(`{"d":4}`), read.ETag); err != nil {
		t.Fatal(err)
	}
}

// TestHTTPStoreUpdateRetriesALostCompareAndSwap is the plane's substitute for
// the file backend's lock: the read and the write are separate round trips, so
// an interleaved writer must cause the body to be RE-RUN against the value
// that actually won — never overwritten.
func TestHTTPStoreUpdateRetriesALostCompareAndSwap(t *testing.T) {
	plane, server := newPlaneServer(t)
	store := planeClient(t, server)

	plane.set(KeyBlockedRecords, []byte(`{"n":1}`))
	var once sync.Once
	plane.interpose = func() {
		// Exactly one interloper, landing in the window between this
		// update's read and its write.
		once.Do(func() { plane.set(KeyBlockedRecords, []byte(`{"n":2}`)) })
	}

	attempts := 0
	var observed []string
	if err := store.Update(t.Context(), KeyBlockedRecords, "op", func(value Value) ([]byte, bool, error) {
		attempts++
		observed = append(observed, string(value.Data))
		return []byte(string(value.Data) + "!"), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("update body ran %d times, want a retry after the lost swap", attempts)
	}
	if observed[0] != `{"n":1}` || observed[1] != `{"n":2}` {
		t.Fatalf("observed = %v, want the retry to see the interloper's value", observed)
	}
	final, _ := plane.get(KeyBlockedRecords)
	if string(final) != `{"n":2}!` {
		t.Fatalf("final = %q, want the retry built on the winner's value (no lost update)", final)
	}
}

// TestHTTPStoreUpdateFailsLoudlyUnderPathologicalContention keeps a hot key
// from spinning forever: the bound is reported, not exceeded silently.
func TestHTTPStoreUpdateFailsLoudlyUnderPathologicalContention(t *testing.T) {
	plane, server := newPlaneServer(t)
	store := planeClient(t, server)

	plane.set(KeyBlockedRecords, []byte(`{"n":0}`))
	churn := 0
	plane.interpose = func() {
		churn++
		plane.set(KeyBlockedRecords, []byte(`{"n":`+strings.Repeat("9", churn)+`}`))
	}

	attempts := 0
	err := store.Update(t.Context(), KeyBlockedRecords, "op", func(value Value) ([]byte, bool, error) {
		attempts++
		return append(value.Data, '!'), true, nil
	})
	if !errors.Is(err, ErrUpdateContention) {
		t.Fatalf("err = %v, want ErrUpdateContention", err)
	}
	if attempts != MaxUpdateAttempts {
		t.Fatalf("attempts = %d, want exactly MaxUpdateAttempts (%d)", attempts, MaxUpdateAttempts)
	}
}

// TestHTTPStoreUpdateSkipsTheWriteWhenNothingChanged keeps the common
// reconcile outcome off the wire entirely.
func TestHTTPStoreUpdateSkipsTheWriteWhenNothingChanged(t *testing.T) {
	plane, server := newPlaneServer(t)
	store := planeClient(t, server)
	plane.set(KeyBlockedRecords, []byte(`{"n":1}`))

	before := plane.requests.Load()
	if err := store.Update(t.Context(), KeyBlockedRecords, "op", func(Value) ([]byte, bool, error) {
		return []byte("ignored"), false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := plane.requests.Load() - before; got != 1 {
		t.Fatalf("%d requests for a no-op update, want just the read", got)
	}
	final, _ := plane.get(KeyBlockedRecords)
	if string(final) != `{"n":1}` {
		t.Fatalf("value = %q, want it untouched", final)
	}
}

// TestHTTPStoreRefusesKeysOutsideTheNamespace fails closed before anything
// reaches the wire — the client is the first of two independent guards.
func TestHTTPStoreRefusesKeysOutsideTheNamespace(t *testing.T) {
	plane, server := newPlaneServer(t)
	store := planeClient(t, server)
	before := plane.requests.Load()

	for _, key := range []string{"claims.json", "../config.yaml", ""} {
		if _, err := store.Get(t.Context(), key); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Get(%q) err = %v, want ErrInvalidKey", key, err)
		}
		if _, err := store.Put(t.Context(), key, []byte("{}"), ""); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Put(%q) err = %v, want ErrInvalidKey", key, err)
		}
	}
	if plane.requests.Load() != before {
		t.Fatal("a refused key still reached the plane")
	}
}

// TestHTTPStoreSurfacesForeignGaggleRefusals pins the containment error the
// daemon answers when a pod addresses a gaggle its run does not belong to: it
// is surfaced as a typed refusal, never degraded into an empty value.
func TestHTTPStoreSurfacesForeignGaggleRefusals(t *testing.T) {
	_, server := newPlaneServer(t)
	store, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "state-token", Gaggle: "someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Get(t.Context(), KeyBlockedRecords)
	if err == nil {
		t.Fatal("a foreign-gaggle read succeeded")
	}
	var planeErr *Error
	if !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden || planeErr.Code != "gaggle_mismatch" {
		t.Fatalf("err = %v, want a typed 403 gaggle_mismatch", err)
	}
}

// TestHTTPStoreRefusesAValueWithoutAnETag fails closed rather than degrading a
// compare-and-swap into a blind overwrite.
func TestHTTPStoreRefusesAValueWithoutAnETag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)
	store := planeClient(t, server)

	if _, err := store.Get(t.Context(), KeyBlockedRecords); err == nil {
		t.Fatal("a value served without an ETag was accepted; a read-modify-write on it would be a blind overwrite")
	}
}

// TestHTTPPriorityTriggerPostsForItsOwnGaggle covers R3's second half from the
// client side: the gaggle is the client's own, never the caller's to choose.
func TestHTTPPriorityTriggerPostsForItsOwnGaggle(t *testing.T) {
	var bodies []string
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/triggers" || request.Method != http.MethodPost {
			http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
			return
		}
		auth = request.Header.Get("Authorization")
		raw, _ := io.ReadAll(request.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runId":"run-9"}`))
	}))
	t.Cleanup(server.Close)
	store := planeClient(t, server)

	for range 2 {
		runID, err := store.PriorityTrigger(t.Context(), "merge-review", "run-1")
		if err != nil {
			t.Fatal(err)
		}
		if runID != "run-9" {
			t.Fatalf("runID = %q", runID)
		}
	}
	if auth != "Bearer state-token" {
		t.Fatalf("authorization = %q, want the same bearer the state route uses", auth)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("retry bodies = %q, want one stable delivery identity", bodies)
	}
	var request triggerRequest
	if err := json.Unmarshal([]byte(bodies[0]), &request); err != nil {
		t.Fatal(err)
	}
	if request.Gaggle != "goobers" || request.Workflow != "merge-review" || request.SourceRun != "run-1" {
		t.Fatalf("request = %+v", request)
	}
	if !strings.HasPrefix(request.RequestID, "priority-") || len(request.RequestID) != len("priority-")+64 {
		t.Fatalf("requestId = %q, want a bounded content-derived delivery identity", request.RequestID)
	}

	if _, err := store.PriorityTrigger(t.Context(), "", "run-1"); err == nil {
		t.Fatal("a priority trigger with no workflow was accepted")
	}
	if _, err := store.PriorityTrigger(t.Context(), "merge-review", ""); err == nil {
		t.Fatal("a priority trigger with no source run was accepted")
	}
}

// TestNewHTTPRefusesAGaggleThatIsNotOnePathElement pins the fail-closed check
// at construction. The gaggle is interpolated into the request path, and the
// HTTP client normalizes "." / ".." / embedded separators away before the
// request leaves the process — so a traversal would silently address some
// other route, whose 404 the caller cannot tell apart from an absent key.
func TestNewHTTPRefusesAGaggleThatIsNotOnePathElement(t *testing.T) {
	t.Parallel()

	for _, gaggle := range []string{"", "   ", ".", "..", "a/b", "goobers/../other", `a\b`} {
		client, err := NewHTTP(HTTPConfig{
			BaseURL: "https://daemon.invalid", Token: "token", Gaggle: gaggle,
		})
		if err == nil {
			t.Fatalf("gaggle %q was accepted: %#v", gaggle, client)
		}
	}

	if _, err := NewHTTP(HTTPConfig{
		BaseURL: "https://daemon.invalid", Token: "token", Gaggle: "goobers",
	}); err != nil {
		t.Fatalf("an ordinary gaggle was refused: %v", err)
	}
}
