package httpapi

import (
	"net/http"
	"time"
)

// LivenessPath and ReadinessPath are the daemon's bare, version-free
// kubelet-facing health surface (#3806). They are matched as literal paths
// ahead of everything else WrapWithProbes wraps, entirely OUTSIDE the
// versioned v1 Router pipeline: no Authenticate, no Authorize, no admission
// control, no per-route budget. That is deliberate, not an oversight — a
// kubelet probe cannot present a pod bearer token or a human credential, so
// these two paths must answer regardless of api.auth configuration
// (including under DenyAllAuthenticator, the posture a non-loopback daemon
// with no configured human authenticator falls back to) and must stay cheap
// and truthful even while the versioned API is admission-shedding under load
// (#1926) — a saturated instance is not an unhealthy one.
//
// Scope is deliberately narrow: each response carries only booleans and
// timestamps, never run/config/credential data, so this fail-open exception
// to the daemon's otherwise fail-closed posture (SEC-043/#640,
// DenyAllAuthenticator, RequireRoles) cannot become a second information
// disclosure surface.
const (
	LivenessPath  = "/healthz"
	ReadinessPath = "/readyz"
)

// LivenessCheck reports whether the daemon's main loop is alive. It must
// stay meaningful even before configuration/definitions ever finish loading
// — a malformed instance.yaml is a startup failure, not a wedged process,
// and must not read as one.
type LivenessCheck func() bool

// ReadinessCheck reports the daemon's overall readiness plus the named
// subsystem checks composing it, for /readyz's JSON body.
type ReadinessCheck func() ReadinessStatus

// ReadinessStatus is the /readyz response body.
//
// Ready is the "plane-ready" gate (#4252, maintainer decision on the same
// issue): true once the HTTP listener and the credential/blob/journal/
// surrender planes are safe to serve, independent of whether crash-resume
// has finished. This is deliberately NOT the same value /api/v1/health's
// Ready field exposes to authenticated callers (that stays the full
// scheduler-ready gate, unchanged) — the split is intentional: a pod already
// running a stage needs to keep reaching those planes for the entire
// crash-resume window, which can be minutes, so the Kubernetes
// Service/readinessProbe must not hold the pod out of rotation for that
// whole window (that was #4252's actual bug). Ready therefore drives this
// endpoint's HTTP status code, and by extension the reference
// deployment's readinessProbe/startupProbe.
//
// SchedulerReady is the fuller "safe to admit new dispatch" gate: crash-
// resume of interrupted runs, plus the initial trigger/claim/cancel/apply
// sweeps, have completed. New scheduling (e.g. the trigger plane minting a
// run from an HTTP-delivered trigger) stays refused until this flips true,
// even though Ready may already be true and the Service may already be
// routing traffic — the pod can safely answer plane requests for
// already-running work without yet being allowed to admit new work.
//
// Checks decomposes both gates into the named subsystem booleans this issue
// asks for (configLoaded, stateOpen, resumeComplete, sweepsStarted for
// cmd/goobers/up.go's daemon); it is diagnostic detail layered on top, not a
// second source of truth — neither Ready nor SchedulerReady is ever
// recomputed from Checks, precisely to avoid the surfaces disagreeing.
type ReadinessStatus struct {
	Ready          bool            `json:"ready"`
	SchedulerReady bool            `json:"schedulerReady"`
	Checks         map[string]bool `json:"checks,omitempty"`
	Startup        *StartupStatus  `json:"startup,omitempty"`
}

// StartupStatus identifies the operation currently blocking daemon readiness.
type StartupStatus struct {
	Phase string    `json:"phase"`
	Since time.Time `json:"since"`
}

// probeHandler serves the two bare probe paths itself and forwards
// everything else — including the authenticatedTransport() and shutdown()
// side-channels Server and apiHandler rely on — straight through to inner
// untouched.
type probeHandler struct {
	inner     http.Handler
	liveness  LivenessCheck
	readiness ReadinessCheck
}

// WrapWithProbes registers LivenessPath and ReadinessPath ahead of inner,
// matched as literal paths before inner ever sees the request — so neither
// route can be shadowed by, or shadow, anything inner itself routes,
// regardless of what inner is (the versioned httpapi.Router today, or a stub
// in a test). Every other path reaches inner exactly as if WrapWithProbes
// were never called.
//
// A nil liveness or readiness check is treated as "always healthy/ready" —
// callers that only care about wiring one of the two probes are not forced
// to stub the other.
func WrapWithProbes(inner http.Handler, liveness LivenessCheck, readiness ReadinessCheck) http.Handler {
	return &probeHandler{inner: inner, liveness: liveness, readiness: readiness}
}

// authenticatedTransport forwards to inner so NewServer's SEC-043
// off-loopback gate (server.go:190-196) still sees straight through this
// wrapper to the real handler's authentication posture — wrapping with
// probes must never look, to that gate, like it made an authenticated API
// accidentally open, nor make a genuinely open one look gated.
func (p *probeHandler) authenticatedTransport() bool {
	return handlerAuthenticated(p.inner)
}

// shutdown forwards to inner's own shutdown hook (apiHandler.shutdown closes
// the SSE event source) if it has one, so wrapping with probes does not leak
// inner's lifecycle resources.
func (p *probeHandler) shutdown() {
	if lifecycle, ok := p.inner.(interface{ shutdown() }); ok {
		lifecycle.shutdown()
	}
}

func (p *probeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case LivenessPath:
		p.serveLiveness(w, r)
	case ReadinessPath:
		p.serveReadiness(w, r)
	default:
		p.inner.ServeHTTP(w, r)
	}
}

func (p *probeHandler) serveLiveness(w http.ResponseWriter, r *http.Request) {
	if !probeMethodAllowed(w, r) {
		return
	}
	healthy := p.liveness == nil || p.liveness()
	status := http.StatusOK
	if !healthy {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, struct {
		Healthy bool `json:"healthy"`
	}{Healthy: healthy})
}

func (p *probeHandler) serveReadiness(w http.ResponseWriter, r *http.Request) {
	if !probeMethodAllowed(w, r) {
		return
	}
	result := ReadinessStatus{Ready: true}
	if p.readiness != nil {
		result = p.readiness()
	}
	status := http.StatusOK
	if !result.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, result)
}

// probeMethodAllowed rejects anything but GET/HEAD with the same structured
// 405 the versioned router returns, so a probe path's error shape is
// unsurprising even though it bypasses the router pipeline entirely.
func probeMethodAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	return false
}
