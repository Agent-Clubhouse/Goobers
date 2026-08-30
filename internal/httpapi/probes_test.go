package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/readservice"
)

// panicHandler proves WrapWithProbes never lets a probe path reach inner:
// if it did, this handler's panic would fail the test.
type panicHandler struct{}

func (panicHandler) ServeHTTP(http.ResponseWriter, *http.Request) {
	panic("inner handler must not see a probe path")
}

// TestProbesBypassAuthenticationUnderDenyAllPosture reproduces #3806's exact
// root cause and this cluster's real deployed posture
// (cluster/goobers-system/instance/instance.yaml: no api.auth block, a
// non-loopback listen address): DenyAllAuthenticator paired with
// RequireRoles refuses every request with no principal, including the
// existing authenticated /api/v1/health route — this is the control proving
// the posture really does 401 a bare kubelet probe today. /healthz and
// /readyz, registered outside the versioned Router pipeline entirely, must
// answer regardless.
func TestProbesBypassAuthenticationUnderDenyAllPosture(t *testing.T) {
	handler, err := NewHandler(
		&fakeReader{health: readservice.Health{Ready: true}},
		RequireRoles(),
		discardLogger(),
		WithAuthenticator(DenyAllAuthenticator{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := WrapWithProbes(handler, func() bool { return true }, func() ReadinessStatus {
		return ReadinessStatus{Ready: true, Checks: map[string]bool{"configLoaded": true}}
	})

	// Control: the existing authenticated route still refuses a bare caller —
	// proving this test exercises the real DenyAll posture, not a permissive
	// stand-in for it.
	response := httptest.NewRecorder()
	wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodGet, HealthPath, nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("authenticated route under DenyAll: status = %d, want %d (test does not reproduce the cluster's posture)", response.Code, http.StatusUnauthorized)
	}

	for _, path := range []string{LivenessPath, ReadinessPath} {
		response := httptest.NewRecorder()
		wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s under DenyAll with no credential: status = %d, body = %s", path, response.Code, response.Body)
		}
	}
}

// TestProbesNeverReachInnerHandler proves the two probe paths are matched
// and answered before inner.ServeHTTP is ever called — the structural
// guarantee behind the admission-control/budget bypass risk flagged for
// #3806: a saturated versioned Router (admission-shed under load, #1926)
// must never make /healthz or /readyz falsely report unhealthy.
func TestProbesNeverReachInnerHandler(t *testing.T) {
	wrapped := WrapWithProbes(panicHandler{}, func() bool { return true }, func() ReadinessStatus {
		return ReadinessStatus{Ready: true}
	})

	for _, path := range []string{LivenessPath, ReadinessPath} {
		response := httptest.NewRecorder()
		wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", path, response.Code, response.Body)
		}
	}

	// Every other path still reaches inner exactly as before wrapping.
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("expected a non-probe path to reach inner and panic")
		}
	}()
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
}

func TestLivenessReflectsCheckResult(t *testing.T) {
	tests := []struct {
		name    string
		healthy bool
		want    int
	}{
		{name: "healthy", healthy: true, want: http.StatusOK},
		{name: "unhealthy", healthy: false, want: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wrapped := WrapWithProbes(panicHandler{}, func() bool { return test.healthy }, nil)
			response := httptest.NewRecorder()
			wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodGet, LivenessPath, nil))
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			var body struct {
				Healthy bool `json:"healthy"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Healthy != test.healthy {
				t.Fatalf("healthy = %t, want %t", body.Healthy, test.healthy)
			}
		})
	}
}

func TestLivenessNilCheckDefaultsHealthy(t *testing.T) {
	wrapped := WrapWithProbes(panicHandler{}, nil, nil)
	response := httptest.NewRecorder()
	wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodGet, LivenessPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestReadinessReflectsNamedSubsystemChecks(t *testing.T) {
	tests := []struct {
		name string
		want ReadinessStatus
		code int
	}{
		{
			name: "not ready",
			want: ReadinessStatus{Ready: false, Checks: map[string]bool{
				"configLoaded": true, "stateOpen": true, "resumeComplete": false, "sweepsStarted": false,
			}},
			code: http.StatusServiceUnavailable,
		},
		{
			name: "ready",
			want: ReadinessStatus{Ready: true, Checks: map[string]bool{
				"configLoaded": true, "stateOpen": true, "resumeComplete": true, "sweepsStarted": true,
			}},
			code: http.StatusOK,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wrapped := WrapWithProbes(panicHandler{}, nil, func() ReadinessStatus { return test.want })
			response := httptest.NewRecorder()
			wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodGet, ReadinessPath, nil))
			if response.Code != test.code {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.code, response.Body)
			}
			var got ReadinessStatus
			if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Ready != test.want.Ready || len(got.Checks) != len(test.want.Checks) {
				t.Fatalf("readiness = %+v, want %+v", got, test.want)
			}
			for name, want := range test.want.Checks {
				if got.Checks[name] != want {
					t.Fatalf("check %q = %t, want %t (full body %+v)", name, got.Checks[name], want, got)
				}
			}
		})
	}
}

func TestReadinessNilCheckDefaultsReady(t *testing.T) {
	wrapped := WrapWithProbes(panicHandler{}, nil, nil)
	response := httptest.NewRecorder()
	wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodGet, ReadinessPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
}

// TestProbesRejectUnsupportedMethod locks the same structured error envelope
// the versioned router returns for a wrong-method request, even though these
// two paths are handled entirely outside that router.
func TestProbesRejectUnsupportedMethod(t *testing.T) {
	wrapped := WrapWithProbes(panicHandler{}, func() bool { return true }, func() ReadinessStatus {
		return ReadinessStatus{Ready: true}
	})
	for _, path := range []string{LivenessPath, ReadinessPath} {
		response := httptest.NewRecorder()
		wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want %d", path, response.Code, http.StatusMethodNotAllowed)
		}
		if allow := response.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Fatalf("%s: Allow = %q", path, allow)
		}
	}
}

// TestProbesForwardAuthenticatedTransportMarker locks the SEC-043 contract:
// NewServer's off-loopback fail-closed gate (server.go:100-113) type-asserts
// the handler it is given for authenticatedTransport() — WrapWithProbes must
// forward that straight through to inner, in both directions, or wrapping a
// handler with probes could silently defeat (or falsely trip) that gate.
func TestProbesForwardAuthenticatedTransportMarker(t *testing.T) {
	authed, err := NewHandler(
		&fakeReader{},
		RequireRoles(),
		discardLogger(),
		WithAuthenticator(DenyAllAuthenticator{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !handlerAuthenticated(WrapWithProbes(authed, nil, nil)) {
		t.Fatal("wrapped authenticated handler must still report authenticatedTransport() = true")
	}

	unauthed, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if handlerAuthenticated(WrapWithProbes(unauthed, nil, nil)) {
		t.Fatal("wrapped tier-1 handler must still report authenticatedTransport() = false")
	}
}

// TestProbesForwardShutdown locks that WrapWithProbes does not swallow
// apiHandler's own shutdown hook (it closes the SSE event source).
func TestProbesForwardShutdown(t *testing.T) {
	called := false
	wrapped := WrapWithProbes(shutdownableHandler{onShutdown: func() { called = true }}, nil, nil)
	lifecycle, ok := wrapped.(interface{ shutdown() })
	if !ok {
		t.Fatal("wrapped handler does not expose shutdown()")
	}
	lifecycle.shutdown()
	if !called {
		t.Fatal("inner shutdown() was not called")
	}
}

// shutdownableHandler is a minimal stand-in for apiHandler: an http.Handler
// that also exposes the shutdown() lifecycle hook WrapWithProbes must
// forward.
type shutdownableHandler struct {
	onShutdown func()
}

func (shutdownableHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

func (h shutdownableHandler) shutdown() { h.onShutdown() }
