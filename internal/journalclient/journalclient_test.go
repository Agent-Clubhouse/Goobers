package journalclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
)

// journalclient_test.go covers the stage-side half of #3880's fail-closed
// posture: which backend a stage picks, and what the client refuses to even
// attempt. The server refuses too — that is internal/httpapi's tests — but a
// boundary with only one side enforcing it is one bug away from open.

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

// TestSelectPrefersTheFilePathWhenNoEndpointIsConfigured is the daemon and
// type-1/type-2 host case: nothing set, nothing changes.
func TestSelectPrefersTheFilePathWhenNoEndpointIsConfigured(t *testing.T) {
	selection, err := Select(env(nil))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.OnPlane() {
		t.Fatalf("selection = %+v, want the file path", selection)
	}

	// A token or run id without an endpoint is still the file path: the
	// endpoint is what says "there is no run directory here".
	selection, err = Select(env(map[string]string{EnvToken: "t", EnvRunID: "run-1"}))
	if err != nil || selection.OnPlane() {
		t.Fatalf("selection = %+v, err = %v; want the file path", selection, err)
	}
}

// TestSelectFailsClosedOnAHalfConfiguredPlane is the whole point of having a
// selection function: an endpoint with no bearer, or no run identity, must be
// a refusal. Falling back to an on-disk journal there is how a pod silently
// reads nothing and a stage silently decides differently.
func TestSelectFailsClosedOnAHalfConfiguredPlane(t *testing.T) {
	if _, err := Select(env(map[string]string{EnvEndpoint: "http://d", EnvRunID: "run-1"})); !errors.Is(err, ErrEndpointWithoutToken) {
		t.Fatalf("err = %v, want ErrEndpointWithoutToken", err)
	}
	if _, err := Select(env(map[string]string{EnvEndpoint: "http://d", EnvToken: "t"})); !errors.Is(err, ErrEndpointWithoutRun) {
		t.Fatalf("err = %v, want ErrEndpointWithoutRun", err)
	}
	// Whitespace is not a token.
	if _, err := Select(env(map[string]string{EnvEndpoint: "http://d", EnvToken: "   ", EnvRunID: "run-1"})); !errors.Is(err, ErrEndpointWithoutToken) {
		t.Fatalf("err = %v, want ErrEndpointWithoutToken for a blank token", err)
	}
}

// TestSelectResolvesAFullyConfiguredPlane pins the happy path's shape.
func TestSelectResolvesAFullyConfiguredPlane(t *testing.T) {
	selection, err := Select(env(map[string]string{
		EnvEndpoint: "http://daemon:7777/", EnvToken: "tok", EnvRunID: "run-1", EnvGaggle: "web",
	}))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !selection.OnPlane() || selection.RunID != "run-1" || selection.Gaggle != "web" || selection.Token != "tok" {
		t.Fatalf("selection = %+v", selection)
	}
}

// TestNewHTTPRefusesAnIncompleteConfiguration keeps the refusal at
// construction rather than at the first read, so a stage cannot get halfway
// through a decision before discovering it has no journal.
func TestNewHTTPRefusesAnIncompleteConfiguration(t *testing.T) {
	for name, cfg := range map[string]HTTPConfig{
		"no base url": {Token: "t", RunID: "run-1"},
		"no token":    {BaseURL: "http://d", RunID: "run-1"},
		"no run":      {BaseURL: "http://d", Token: "t"},
		"bad run":     {BaseURL: "http://d", Token: "t", RunID: "../../etc/passwd"},
	} {
		if _, err := NewHTTP(cfg); err == nil {
			t.Errorf("%s: NewHTTP succeeded, want a refusal", name)
		}
	}
}

// TestHTTPAddressesOnlyItsOwnRun is client-side containment: the run in every
// same-run request comes from the client's own configuration, never from a
// caller, so a bug cannot even ATTEMPT a read the daemon would have to refuse.
func TestHTTPAddressesOnlyItsOwnRun(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("authorization header = %q", got)
		}
		_, _ = w.Write([]byte(`{"runId":"run-1","events":[],"attempts":[]}`))
	}))
	defer server.Close()

	client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1", Gaggle: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Events(); err != nil {
		t.Fatalf("events: %v", err)
	}
	if _, err := client.StageAttempts("implement"); err != nil {
		t.Fatalf("stage attempts: %v", err)
	}
	for _, path := range paths {
		if !strings.Contains(path, "/runs/run-1/") && !strings.HasSuffix(path, "/runs/run-1/events") {
			t.Errorf("request path %q left this client's own run", path)
		}
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want two requests", paths)
	}
}

// TestHTTPRefusesAnAnswerForTheWrongRun is the other half of that: if the
// daemon ever answers with a different run's list, the client rejects it
// rather than acting on it.
func TestHTTPRefusesAnAnswerForTheWrongRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"runId":"run-99","events":[]}`))
	}))
	defer server.Close()

	client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Events(); err == nil {
		t.Fatal("an event list for another run was accepted")
	}
}

// TestCrossRunReadsRequireAGaggleScope pins the fail-closed scope rule on the
// client too: an unscoped cross-run walk is what decision 005 R1 declined to
// expose, so the client will not ask for one.
func TestCrossRunReadsRequireAGaggleScope(t *testing.T) {
	var served int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunPhase(t.Context(), "run-9"); !errors.Is(err, ErrGaggleRequired) {
		t.Fatalf("run phase err = %v, want ErrGaggleRequired", err)
	}
	if _, err := client.ConflictTouches(t.Context(), ConflictTouchRequest{}); !errors.Is(err, ErrGaggleRequired) {
		t.Fatalf("conflict touches err = %v, want ErrGaggleRequired", err)
	}
	if _, err := client.UnpushedWork(t.Context(), UnpushedWorkRequest{}); !errors.Is(err, ErrGaggleRequired) {
		t.Fatalf("unpushed work err = %v, want ErrGaggleRequired", err)
	}
	if served != 0 {
		t.Fatalf("%d unscoped cross-run requests were sent", served)
	}
}

// TestPlaneRefusalsSurfaceAsErrorsNotEmptyAnswers is the anti-silent-
// degradation guarantee: every refusal the daemon can return has to reach the
// caller as an error. An empty result would be read as "there is nothing",
// which for the failure streak and the hot-file map is a policy change.
func TestPlaneRefusalsSurfaceAsErrorsNotEmptyAnswers(t *testing.T) {
	for _, status := range []int{
		http.StatusForbidden, http.StatusNotFound, http.StatusUnauthorized,
		http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusTooManyRequests,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"run_mismatch","message":"nope"}}`))
		}))

		client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1", Gaggle: "web"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Events(); err == nil {
			t.Errorf("status %d: Events returned no error", status)
		}
		if _, err := client.RunPhase(t.Context(), "run-9"); err == nil {
			t.Errorf("status %d: RunPhase returned no error", status)
		} else {
			var planeErr *Error
			if !errors.As(err, &planeErr) || planeErr.Status != status || planeErr.Code != "run_mismatch" {
				t.Errorf("status %d: err = %v, want a typed plane Error carrying the envelope", status, err)
			}
		}
		if _, err := client.ConflictTouches(t.Context(), ConflictTouchRequest{Gaggle: "web"}); err == nil {
			t.Errorf("status %d: ConflictTouches returned no error", status)
		}
		if _, err := client.UnpushedWork(t.Context(), UnpushedWorkRequest{Gaggle: "web"}); err == nil {
			t.Errorf("status %d: UnpushedWork returned no error", status)
		}
		server.Close()
	}
}

// TestRunPhaseRefusesAnEmptyAnswer guards the one shape a well-meaning daemon
// could return that would silently zero the failure streak.
func TestRunPhaseRefusesAnEmptyAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"runId":"run-9","phase":""}`))
	}))
	defer server.Close()

	client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1", Gaggle: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunPhase(t.Context(), "run-9"); err == nil {
		t.Fatal("an empty phase was accepted; it must be an explicit error")
	}
}

// TestArtifactReadsRejectMalformedDigests keeps a bad digest from ever
// becoming a request path.
func TestArtifactReadsRejectMalformedDigests(t *testing.T) {
	var served int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		_, _ = w.Write(nil)
	}))
	defer server.Close()

	client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{"", "   ", "../../etc/passwd", "sha256:short", "notadigest"} {
		if _, err := client.ArtifactByDigest(digest); err == nil {
			t.Errorf("digest %q was accepted", digest)
		}
	}
	if served != 0 {
		t.Fatalf("%d malformed-digest requests were sent", served)
	}
}

// TestArtifactFetchBoundsTheBodyAtTheTransport is the pod's memory guard: a
// daemon that answers a bounded artifact request with an unbounded body must
// be cut off as it arrives, not after the whole thing has been buffered and
// then found too large.
func TestArtifactFetchBoundsTheBodyAtTheTransport(t *testing.T) {
	oversized := strings.Repeat("x", 64<<10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oversized))
	}))
	defer server.Close()

	client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	ref := journal.Ref{Digest: journal.Digest([]byte(oversized))}
	if _, err := client.ArtifactBytesBounded(ref, 1024); err == nil {
		t.Fatal("a 64KiB body was accepted under a 1KiB bound")
	}
	// The same body inside the bound still verifies and is returned.
	data, err := client.ArtifactBytesBounded(ref, int64(len(oversized)))
	if err != nil {
		t.Fatalf("within-bound fetch: %v", err)
	}
	if len(data) != len(oversized) {
		t.Fatalf("len = %d, want %d", len(data), len(oversized))
	}
}

// TestArtifactFetchRefusesASubstitutedArtifact covers both ways the daemon
// can answer with content other than what was asked for: naming a different
// digest in the response header, and simply serving different bytes. Neither
// may reach the caller — a stage that confirms evidence against an artifact
// must fail loudly rather than confirm against a substitute.
func TestArtifactFetchRefusesASubstitutedArtifact(t *testing.T) {
	wanted := journal.Digest([]byte("the real signals output"))
	other := journal.Digest([]byte("something else entirely"))

	t.Run("header names another digest", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(apicontract.DigestHeader, other)
			_, _ = w.Write([]byte("the real signals output"))
		}))
		defer server.Close()
		client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.ArtifactByDigest(wanted); err == nil || !strings.Contains(err.Error(), other) {
			t.Fatalf("err = %v, want a refusal naming the substituted digest", err)
		}
	})

	t.Run("body is not what the digest addresses", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("fabricated signals output"))
		}))
		defer server.Close()
		client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.ArtifactByDigest(wanted); err == nil {
			t.Fatal("substituted content was accepted")
		}
	})
}
