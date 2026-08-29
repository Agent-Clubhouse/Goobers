// Package httpapi exposes the versioned loopback HTTP adapter: read routes
// over readservice.Reader, plus the tier-2 human-intervention mutation routes
// (approve/override/rerun — HITL-7/#469).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

const (
	// Prefix is the versioned root for all HTTP API routes.
	Prefix = apicontract.V1Prefix
	// HealthPath is the daemon health endpoint.
	HealthPath = apicontract.HealthPath
	// TelemetryStatsPath exposes workflow and stage telemetry aggregates.
	TelemetryStatsPath = apicontract.TelemetryStatsPath
	// TelemetryErrorSignaturesPath exposes recurring error code/class aggregates.
	TelemetryErrorSignaturesPath = apicontract.TelemetryErrorSignaturesPath
	// TelemetryErrorsPath exposes paginated recent telemetry errors.
	TelemetryErrorsPath = apicontract.TelemetryErrorsPath
	// RunsPath is the run history endpoint.
	RunsPath = apicontract.RunsPath
	// InstancePath is the instance inventory endpoint.
	InstancePath = apicontract.InstancePath
	// PortalConfigPath is the dashboard co-brand config endpoint.
	PortalConfigPath = apicontract.PortalConfigPath
	// GagglesPath is the gaggle inventory endpoint.
	GagglesPath = apicontract.GagglesPath
	// GaggleGoobersPath is the gaggle-scoped goober inventory route.
	GaggleGoobersPath = apicontract.GaggleGoobersPath
	// GaggleWorkflowsPath is the gaggle-scoped workflow inventory route.
	GaggleWorkflowsPath = apicontract.GaggleWorkflowsPath
	// GaggleConnectionsPath is the gaggle-scoped repository connection route.
	GaggleConnectionsPath = apicontract.GaggleConnectionsPath
	// WorkflowDetailPath is the gaggle-scoped workflow detail route.
	WorkflowDetailPath = apicontract.WorkflowDetailPath
	// EventsPath is the resumable SSE read-model invalidation stream.
	EventsPath = apicontract.EventsPath
)

// Authorizer preserves the authorization boundary for every API route. Tier 1
// supplies AllowAll; later tiers can replace it without changing handlers.
type Authorizer interface {
	Authorize(*http.Request) error
}

// Principal is the identity established by an Authenticator.
type Principal struct {
	Subject string
	// Issuer identifies the trust domain that authenticated Subject.
	Issuer string
	// Name is a human-readable display claim when the issuer provides one.
	Name string
	// Roles are the instance-scoped roles granted to this principal by
	// configuration. Empty means authenticated but authorized for nothing.
	Roles []Role
}

// Role is an instance-scoped authorization level (#644). Roles are ordered:
// admin implies operate, operate implies view.
type Role string

// Instance roles, weakest first.
const (
	RoleView    Role = "view"
	RoleOperate Role = "operate"
	RoleAdmin   Role = "admin"
)

func roleRank(role Role) int {
	switch role {
	case RoleView:
		return 1
	case RoleOperate:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// HasRole reports whether the principal holds required or a stronger role.
// Unknown role values never satisfy anything, so a mangled grant fails closed.
func (p Principal) HasRole(required Role) bool {
	need := roleRank(required)
	if need == 0 {
		return false
	}
	for _, role := range p.Roles {
		if roleRank(role) >= need {
			return true
		}
	}
	return false
}

// PodPrincipalIssuer marks a principal authenticated through the pod-to-daemon
// seam (a per-run bearer minted by the daemon; internal/podauth is the v1
// implementation). Pod principals hold no instance roles: authorization for
// them is plane-scoped, not role-ranked.
const PodPrincipalIssuer = "goobers/pod"

// IsPodPrincipal reports whether principal was authenticated as a stage pod.
func IsPodPrincipal(principal Principal) bool {
	return principal.Issuer == PodPrincipalIssuer
}

// podPlanePath reports whether path is one of the write routes a pod
// principal may reach: the claims plane (its four mutations and its list
// read, all POST), the credential plane's resolve route
// (distributed-state-and-coordination.md §11 — the plane exists FOR stage
// pods), and the cross-run journal plane's three purpose-built questions
// (decision 005 R1 / finding 002 C4 — each answered by the daemon from data
// the daemon derives, gaggle-scoped, never another run's journal). The
// journal plane is the third pod-reachable plane; it carries a run-id segment
// and is matched structurally by journalPlanePath. The blob plane is the
// fourth; it carries a digest segment and is matched structurally by
// blobPlanePath. The surrender plane is the fifth; it carries
// run/stage/attempt segments and is matched structurally by
// surrenderPlanePath. The run-scoped READ routes are the sixth (decision 005
// R1 option 1); they carry a run-id segment and are matched structurally by
// runReadPlanePath. Everything else (other reads, triggers, HITL,
// interventions) stays human-only: a stage pod has no business resolving
// escalations or minting runs.
func podPlanePath(path string) bool {
	switch path {
	case apicontract.ClaimAcquirePath,
		apicontract.ClaimRenewPath,
		apicontract.ClaimReleasePath,
		apicontract.ClaimSettlePath,
		apicontract.ClaimListPath,
		apicontract.CredentialResolvePath,
		apicontract.JournalRunPhasePath,
		apicontract.JournalConflictTouchesPath,
		apicontract.JournalUnpushedWorkPath:
		return true
	default:
		return false
	}
}

// runReadPlanePath reports whether path is one of the THREE run-scoped read
// routes decision 005 ruling R1 (option 1) admits a pod principal to: its own
// run's events, one of its own run's stages' attempts, and one of its own
// run's artifacts by digest. Matched structurally because each carries a
// run-id segment; WHICH run is enforced by the handlers (podRunContained),
// which can see both the path run id and the principal — the same division
// the journal-emit and surrender planes use.
//
// Deliberately NOT admitted: RunDetailPath (a run summary is a portal view,
// and no converted reader needs it), RunTranscriptPath (raw agent transcripts
// — outside the enumerated ruling, so refused), RunsPath (a list of every run
// on the instance is not a same-run read at all), and RunRevealPath (a local
// host action). Fail closed: the ruling enumerated three routes, so three is
// what this admits.
func runReadPlanePath(path string) bool {
	rest, ok := strings.CutPrefix(path, apicontract.RunsPath+"/")
	if !ok {
		return false
	}
	segments := strings.Split(rest, "/")
	switch len(segments) {
	case 2:
		return segments[0] != "" && segments[1] == "events"
	case 3:
		return segments[0] != "" && segments[1] == "artifacts" && segments[2] != ""
	case 4:
		run, stagesLiteral, stage, attemptsLiteral := segments[0], segments[1], segments[2], segments[3]
		return run != "" && stagesLiteral == "stages" && stage != "" && attemptsLiteral == "attempts"
	default:
		return false
	}
}

// journalPlanePath reports whether path is the journal plane's emit route
// (§8, DS4) — the second pod-reachable plane. Matched structurally because
// the route carries a run-id segment; Handler() has already refused
// non-clean paths, so segment counting is sound. Which run the pod may emit
// into is enforced by the handler, which can see both the path run id and
// the principal.
func journalPlanePath(path string) bool {
	rest, ok := strings.CutPrefix(path, apicontract.RunsPath+"/")
	if !ok {
		return false
	}
	run, ok := strings.CutSuffix(rest, "/journal/emit")
	if !ok {
		return false
	}
	return run != "" && !strings.Contains(run, "/")
}

// blobDigestPrefix is BlobDigestPath with its "{digest}" wildcard trimmed —
// the literal prefix every blob-plane request path starts with. Derived from
// the contract constant rather than restated, so the two cannot drift.
var blobDigestPrefix = strings.TrimSuffix(apicontract.BlobDigestPath, "{digest}")

// blobPlanePath reports whether path is the blob plane's digest route
// (decision 010/012, §2a) — the fourth pod-reachable plane. Matched
// structurally, like the journal plane, because the route carries a digest
// segment rather than a fixed suffix.
func blobPlanePath(path string) bool {
	return strings.HasPrefix(path, blobDigestPrefix) && len(path) > len(blobDigestPrefix)
}

// surrenderPlanePath reports whether path is the surrender plane's PUT route
// (#3699) — the fifth pod-reachable plane. Matched structurally, like the
// journal plane, because the route carries run/stage/attempt segments; which
// run the pod may surrender into is enforced by the handler, exactly as the
// journal plane enforces it.
func surrenderPlanePath(path string) bool {
	rest, ok := strings.CutPrefix(path, apicontract.RunsPath+"/")
	if !ok {
		return false
	}
	segments := strings.Split(rest, "/")
	if len(segments) != 6 {
		return false
	}
	run, stagesLiteral, stage, attemptsLiteral, attempt, suffix := segments[0], segments[1], segments[2], segments[3], segments[4], segments[5]
	if stagesLiteral != "stages" || attemptsLiteral != "attempts" || suffix != "surrender" {
		return false
	}
	return run != "" && stage != "" && attempt != ""
}

// RequireRoles authorizes read requests (GET/HEAD) for principals holding
// view or stronger and every other method for operate or stronger. Requests
// without an authenticated principal are denied, so this authorizer must be
// paired with a real Authenticator — under NullAuthenticator every request
// stays anonymous and would be refused.
//
// Pod principals (PodPrincipalIssuer) bypass the role ladder and are confined
// to the pod planes (claims + credential resolve + journal emit + blob
// get/put + surrender put + the cross-run journal plane + their OWN run's
// three read routes): their token proves "I am run X's stage pod", which
// authorizes ledger operations, credential resolution, journal emission, blob
// transfer, result surrender, and reads of run X's own scrubbed journal — and
// nothing else. Per-run containment (the request body's runId, or the path
// run id for the journal, surrender, and run-read routes, matching the pod's
// run) is enforced by the plane handlers, which can see the request — the
// credential and blob handlers additionally refuse human principals outright
// (DS9: those planes serve stage pods only; the blob plane's digest carries
// no run to compare against).
func RequireRoles() Authorizer {
	return authorizerFunc(func(request *http.Request) error {
		principal, ok := PrincipalFromRequest(request)
		if !ok {
			return errors.New("no authenticated principal")
		}
		if IsPodPrincipal(principal) {
			if (podPlanePath(request.URL.Path) || journalPlanePath(request.URL.Path) || surrenderPlanePath(request.URL.Path)) && request.Method == http.MethodPost {
				return nil
			}
			if blobPlanePath(request.URL.Path) && (request.Method == http.MethodGet || request.Method == http.MethodPut) {
				return nil
			}
			// Decision 005 R1 option 1: reads of the pod's own run's journal,
			// GET only. A pod may never write through a read route, and the
			// handler still decides WHICH run.
			if runReadPlanePath(request.URL.Path) && request.Method == http.MethodGet {
				return nil
			}
			return fmt.Errorf("pod principal %q may only call the claims, credential, journal, blob, and surrender planes and read its own run", principal.Subject)
		}
		required := RoleView
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			required = RoleOperate
		}
		if !principal.HasRole(required) {
			return fmt.Errorf("principal %q does not hold the %s role", principal.Subject, required)
		}
		return nil
	})
}

// Authenticator establishes the caller identity before authorization.
type Authenticator interface {
	Authenticate(*http.Request) (*Principal, error)
}

// NullAuthenticator is the tier-1 local-trust authenticator. It requires no
// identity and leaves the request anonymous.
type NullAuthenticator struct{}

// Authenticate accepts anonymous local requests.
func (NullAuthenticator) Authenticate(*http.Request) (*Principal, error) { return nil, nil }

type principalContextKey struct{}

// PrincipalFromRequest returns the identity established for a request.
func PrincipalFromRequest(request *http.Request) (Principal, bool) {
	if request == nil {
		return Principal{}, false
	}
	principal, ok := request.Context().Value(principalContextKey{}).(Principal)
	return principal, ok
}

// DenyAllAuthenticator refuses every request. It exists so a daemon with NO
// human API surface can still satisfy the non-loopback authenticator
// requirement (SEC-043/#640) while serving only its own stage pods, which
// authenticate ahead of it via podauth. Denying is the correct answer for a
// human request to such a daemon — the alternative today is to configure an
// unrelated OIDC issuer purely to unlock pod traffic, which makes the
// most-restrictive deployment the hardest one to express (Goobers#3701).
type DenyAllAuthenticator struct{}

// Authenticate always fails closed.
func (DenyAllAuthenticator) Authenticate(*http.Request) (*Principal, error) {
	return nil, errors.New("httpapi: this daemon exposes no human API surface; only stage-pod tokens are accepted")
}

type authorizerFunc func(*http.Request) error

func (f authorizerFunc) Authorize(r *http.Request) error { return f(r) }

// AllowAll is the tier-1 local-trust authorizer.
var AllowAll Authorizer = authorizerFunc(func(*http.Request) error { return nil })

// ErrorEnvelope is the single error shape returned by every API route.
type ErrorEnvelope = apicontract.ErrorEnvelope

// APIError is a stable machine code and safe human-readable message.
type APIError = apicontract.APIError

// Router registers versioned contract routes behind an Authorizer.
type Router struct {
	mux           *http.ServeMux
	authenticator Authenticator
	authorizer    Authorizer
	routes        []apicontract.Route

	// admission bounds concurrency per cost class (#1926). Lazily created so
	// every existing Router construction keeps working without threading a
	// constructor argument through each one.
	admissionOnce sync.Once
	admission     *admissionController
}

// ensureAdmission creates the controller on first use.
func (r *Router) ensureAdmission() {
	r.admissionOnce.Do(func() { r.admission = newAdmissionController() })
}

type handlerConfig struct {
	events              eventSource
	authenticator       Authenticator
	interventions       InterventionService
	interventionContext context.Context
	runRevealer         func(context.Context, string) error
	claims              ClaimService
	triggers            TriggerService
	escalations         EscalationService
	journal             JournalService
	runJournal          RunJournalService
	credentials         CredentialService
	blobs               blobstore.Store
	surrenders          SurrenderService
}

// HandlerOption configures optional HTTP transport surfaces.
type HandlerOption func(*handlerConfig) error

// WithChangeFeedStream registers the SSE endpoint backed by the read model's
// change feed (#1929).
//
// The only SSE source (#1929). It tails rows the projector wrote in the same
// transaction as the facts they describe, so it is ordered, durable, and
// bounded by active work.
//
// A topology with no read model registers no stream at all rather than falling
// back to a second detector; the freshness surface reports that as degraded.
func WithChangeFeedStream(store *readmodel.Store) HandlerOption {
	return func(h *handlerConfig) error {
		h.events = newFeedStream(store)
		return nil
	}
}

// WithAuthenticator replaces the tier-1 NullAuthenticator.
func WithAuthenticator(authenticator Authenticator) HandlerOption {
	return func(config *handlerConfig) error {
		if authenticator == nil {
			return errors.New("http API authenticator is required")
		}
		config.authenticator = authenticator
		return nil
	}
}

// WithInterventions enables the human-intervention mutation handlers.
func WithInterventions(interventions InterventionService) HandlerOption {
	return func(config *handlerConfig) error {
		if interventions == nil {
			return errors.New("http API intervention service is required")
		}
		config.interventions = interventions
		return nil
	}
}

// WithInterventionContext binds accepted mutations to the daemon lifecycle
// rather than the requesting client's connection lifetime.
func WithInterventionContext(ctx context.Context) HandlerOption {
	return func(config *handlerConfig) error {
		if ctx == nil {
			return errors.New("http API intervention context is required")
		}
		config.interventionContext = ctx
		return nil
	}
}

// WithRunRevealer enables the local-only action that opens a run directory in
// the host file browser.
func WithRunRevealer(reveal func(context.Context, string) error) HandlerOption {
	return func(config *handlerConfig) error {
		if reveal == nil {
			return errors.New("run revealer is required")
		}
		config.runRevealer = reveal
		return nil
	}
}

type apiHandler struct {
	http.Handler
	events        eventSource
	authenticated bool
}

func (h *apiHandler) shutdown() {
	if h.events != nil {
		h.events.Close()
	}
}

// authenticatedTransport reports whether a real (non-null) Authenticator
// gates this handler. NewServer consults it for the off-loopback fail-closed
// startup rule (#640).
func (h *apiHandler) authenticatedTransport() bool { return h.authenticated }

func newRouter(authenticator Authenticator, authorizer Authorizer) (*Router, error) {
	if authenticator == nil {
		return nil, errors.New("http API authenticator is required")
	}
	if authorizer == nil {
		return nil, errors.New("http API authorizer is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	return &Router{mux: mux, authenticator: authenticator, authorizer: authorizer}, nil
}

// Handle registers a typed contract route. Other methods receive the structured
// error envelope rather than net/http's plain-text method error.
func (r *Router) Handle(routeID apicontract.RouteID, handler http.HandlerFunc) {
	r.ensureAdmission()
	route, ok := apicontract.V1Route(routeID)
	if !ok {
		panic(fmt.Sprintf("unknown API route ID %q", routeID))
	}
	r.routes = append(r.routes, route)
	r.mux.HandleFunc(route.Path, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != route.Method {
			w.Header().Set("Allow", route.Method)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.serve(route, handler, w, request)
	})
}

// HandleByMethod registers several routes that share one literal path,
// dispatching on request method — needed only where more than one HTTP method
// addresses the same resource. In the v1 contract that is exactly the blob
// plane's digest route (RouteBlobGet/RouteBlobPut both carry BlobDigestPath):
// Handle's model is one path per route, and net/http's ServeMux rejects two
// bare registrations of an identical pattern, so a shared path needs its own
// registration path rather than two Handle calls.
//
// Every entry's Route (cost class, budget, action class) governs its own
// request exactly as it would under Handle; a request whose method matches no
// entry gets the same structured 405 a single-method route returns for the
// wrong verb, with Allow naming every method actually registered.
func (r *Router) HandleByMethod(routeIDsByMethod map[string]apicontract.RouteID, handlers map[apicontract.RouteID]http.HandlerFunc) {
	if len(routeIDsByMethod) == 0 {
		panic("HandleByMethod requires at least one route")
	}
	r.ensureAdmission()
	byMethod := make(map[string]apicontract.Route, len(routeIDsByMethod))
	var sharedPath string
	allowed := make([]string, 0, len(routeIDsByMethod))
	for method, routeID := range routeIDsByMethod {
		route, ok := apicontract.V1Route(routeID)
		if !ok {
			panic(fmt.Sprintf("unknown API route ID %q", routeID))
		}
		if route.Method != method {
			panic(fmt.Sprintf("route %q is registered under method %q but declares method %q", routeID, method, route.Method))
		}
		if handlers[routeID] == nil {
			panic(fmt.Sprintf("route %q has no handler", routeID))
		}
		if sharedPath == "" {
			sharedPath = route.Path
		} else if route.Path != sharedPath {
			panic(fmt.Sprintf("route %q path %q does not match shared path %q", routeID, route.Path, sharedPath))
		}
		byMethod[method] = route
		r.routes = append(r.routes, route)
		allowed = append(allowed, method)
	}
	sort.Strings(allowed)
	allowHeader := strings.Join(allowed, ", ")
	r.mux.HandleFunc(sharedPath, func(w http.ResponseWriter, request *http.Request) {
		route, ok := byMethod[request.Method]
		if !ok {
			w.Header().Set("Allow", allowHeader)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.serve(route, handlers[route.ID], w, request)
	})
}

// serve runs the per-request pipeline shared by every registered route —
// authenticate, authorize, admit, bound — then calls handler. Factored out of
// Handle so HandleByMethod's multi-method dispatch reuses it exactly rather
// than re-implementing auth/admission/budget a second way.
func (r *Router) serve(route apicontract.Route, handler http.HandlerFunc, w http.ResponseWriter, request *http.Request) {
	principal, err := r.authenticator.Authenticate(request)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "request is not authenticated")
		return
	}
	if principal != nil {
		request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, *principal))
	}
	if err := r.authorizer.Authorize(request); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", "request is not authorized")
		return
	}
	// Admission control (#1926). Applied AFTER auth for the same reason the
	// budget is — an unauthenticated request must not consume a slot — and
	// BEFORE the budget, because a refused request should not have started
	// its budget clock at all.
	//
	// Shed at admission rather than accept-and-timeout: queue wait counts
	// against the budget, so a saturated class that accepts work it cannot
	// finish burns the caller's whole budget and returns nothing anyway.
	if release, admitted := r.admission.admit(route.Cost); admitted {
		defer release()
	} else {
		writeAdmissionRefusal(w, route.Cost)
		return
	}
	// Bound the request (#1917). Applied here rather than per handler so a
	// route cannot be added without one, and applied AFTER auth so an
	// unauthenticated request is rejected without consuming budget.
	if budget, bounded := routeBudget(route.ID); bounded {
		bounded, cancel := withBudget(w, request, budget)
		defer cancel()
		request = bounded
	}
	handler(w, request)
}

// Handler returns the registered routes with a structured unknown-route
// fallback.
func (r *Router) Handler() http.Handler {
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != path.Clean(request.URL.Path) {
			notFound.ServeHTTP(w, request)
			return
		}
		_, pattern := r.mux.Handler(request)
		if pattern == "" {
			notFound.ServeHTTP(w, request)
			return
		}
		r.mux.ServeHTTP(w, request)
	})
}

// NewHandler registers the v1 read routes over the shared service.
func NewHandler(reader readservice.Reader, authorizer Authorizer, errorLog *log.Logger, opts ...HandlerOption) (http.Handler, error) {
	if reader == nil {
		return nil, errors.New("http API read service is required")
	}
	if errorLog == nil {
		return nil, errors.New("http API error logger is required")
	}
	config := handlerConfig{
		authenticator:       NullAuthenticator{},
		interventionContext: context.Background(),
	}
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return nil, err
		}
	}
	router, err := newRouter(config.authenticator, authorizer)
	if err != nil {
		return nil, err
	}
	registerV1Routes(router, reader, errorLog, config)
	// The event stream is optional wiring, so the events route is only part of
	// what this handler must serve when a stream is actually configured.
	expected := apicontract.V1Routes()
	if config.events != nil {
		registerEventRoute(router, config.events)
	} else {
		expected = slices.DeleteFunc(expected, func(route apicontract.Route) bool {
			return route.ID == apicontract.RouteEvents
		})
	}
	if err := apicontract.ValidateRoutes(expected, router.routes); err != nil {
		return nil, fmt.Errorf("register HTTP API routes: %w", err)
	}
	_, isNull := config.authenticator.(NullAuthenticator)
	return &apiHandler{Handler: router.Handler(), events: config.events, authenticated: !isNull}, nil
}

func registerV1Routes(router *Router, reader readservice.Reader, errorLog *log.Logger, config handlerConfig) {
	router.Handle(apicontract.RouteHealth, func(w http.ResponseWriter, request *http.Request) {
		health, err := reader.Health(request.Context())
		if err != nil {
			errorLog.Printf("health read failed: %v", err)
			writeError(w, http.StatusInternalServerError, "read_error", "runtime state could not be read")
			return
		}
		writeJSON(w, http.StatusOK, health)
	})
	router.Handle(apicontract.RoutePortalConfig, func(w http.ResponseWriter, request *http.Request) {
		portalConfig, err := reader.PortalConfig(request.Context())
		if err != nil {
			errorLog.Printf("portal config read failed: %v", err)
			writeError(w, http.StatusInternalServerError, "read_error", "portal config could not be read")
			return
		}
		portalConfig.Capabilities.RevealRun = config.runRevealer != nil
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, portalConfig)
	})
	registerTelemetryRoutes(router, reader, errorLog)
	registerRunRoutes(router, reader, errorLog)
	registerInventoryRoutes(router, reader, errorLog)
	registerMutationRoutes(router, config.interventions, config.interventionContext, errorLog)
	registerRunRevealRoute(router, config.runRevealer, errorLog)
	registerWritePlaneRoutes(router, config, errorLog)
	registerJournalPlaneRoutes(router, config, errorLog)
	registerRunJournalPlaneRoutes(router, config, errorLog)
	registerBlobPlaneRoutes(router, config.blobs, errorLog)
	registerSurrenderPlaneRoutes(router, config, errorLog)
}

func registerRunRevealRoute(router *Router, reveal func(context.Context, string) error, errorLog *log.Logger) {
	router.Handle(apicontract.RouteRunReveal, func(w http.ResponseWriter, request *http.Request) {
		if reveal == nil {
			writeError(w, http.StatusNotFound, "not_available", "run reveal is not available for this deployment")
			return
		}
		if err := reveal(request.Context(), request.PathValue("run")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, "not_found", "requested run data was not found")
				return
			}
			errorLog.Printf("reveal run failed: %v", err)
			writeError(w, http.StatusInternalServerError, "reveal_error", "run directory could not be opened")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func registerRunRoutes(router *Router, reader readservice.Reader, errorLog *log.Logger) {
	router.Handle(apicontract.RouteRuns, func(w http.ResponseWriter, request *http.Request) {
		options, err := runListOptions(request)
		if err != nil {
			writeReadError(w, errorLog, "list runs", err)
			return
		}
		runs, err := reader.ListRuns(request.Context(), options)
		if err != nil {
			writeReadError(w, errorLog, "list runs", err)
			return
		}
		writeJSON(w, http.StatusOK, runs)
	})
	router.Handle(apicontract.RouteRunDetail, func(w http.ResponseWriter, request *http.Request) {
		run, err := reader.GetRun(request.Context(), request.PathValue("run"))
		if err != nil {
			writeReadError(w, errorLog, "get run", err)
			return
		}
		writeJSON(w, http.StatusOK, run)
	})
	router.Handle(apicontract.RouteRunEvents, func(w http.ResponseWriter, request *http.Request) {
		run := request.PathValue("run")
		if !podRunContained(w, request, run, "events") {
			return
		}
		events, err := reader.RunEvents(request.Context(), run)
		if err != nil {
			writeReadError(w, errorLog, "read run events", err)
			return
		}
		writeJSON(w, http.StatusOK, events)
	})
	router.Handle(apicontract.RouteStageAttempts, func(w http.ResponseWriter, request *http.Request) {
		run := request.PathValue("run")
		if !podRunContained(w, request, run, "stage attempts") {
			return
		}
		attempts, err := reader.StageAttempts(
			request.Context(),
			run,
			request.PathValue("stage"),
		)
		if err != nil {
			writeReadError(w, errorLog, "read stage attempts", err)
			return
		}
		writeJSON(w, http.StatusOK, attempts)
	})
	router.Handle(apicontract.RouteRunArtifact, func(w http.ResponseWriter, request *http.Request) {
		run := request.PathValue("run")
		if !podRunContained(w, request, run, "artifacts") {
			return
		}
		artifact, err := reader.Artifact(
			request.Context(),
			run,
			request.PathValue("digest"),
		)
		if err != nil {
			writeReadError(w, errorLog, "read artifact", err)
			return
		}
		w.Header().Set("Content-Type", artifact.Metadata.MediaType)
		w.Header().Set("Content-Length", strconv.Itoa(len(artifact.Bytes)))
		w.Header().Set("ETag", `"`+artifact.Metadata.Digest+`"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Goobers-Digest", artifact.Metadata.Digest)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(artifact.Bytes)
	})
	router.Handle(apicontract.RouteRunTranscript, func(w http.ResponseWriter, request *http.Request) {
		seq, err := strconv.ParseUint(request.PathValue("seq"), 10, 64)
		if err != nil || seq == 0 {
			writeReadError(w, errorLog, "read transcript", fmt.Errorf("%w: invalid transcript sequence", readservice.ErrInvalidArgument))
			return
		}
		transcript, err := reader.Transcript(
			request.Context(),
			request.PathValue("run"),
			seq,
		)
		if err != nil {
			writeReadError(w, errorLog, "read transcript", err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(transcript.Bytes)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Goobers-Event-Sequence", strconv.FormatUint(transcript.Seq, 10))
		w.Header().Set("X-Goobers-Stage", transcript.Stage)
		w.Header().Set("X-Goobers-Transcript-Name", transcript.Name)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(transcript.Bytes)
	})
}

func runListOptions(request *http.Request) (readservice.RunListOptions, error) {
	query := request.URL.Query()
	since, err := parseOptionalTime(query.Get("since"), "since")
	if err != nil {
		return readservice.RunListOptions{}, fmt.Errorf("%w: %w", readservice.ErrInvalidArgument, err)
	}
	until, err := parseOptionalTime(query.Get("until"), "until")
	if err != nil {
		return readservice.RunListOptions{}, fmt.Errorf("%w: %w", readservice.ErrInvalidArgument, err)
	}
	latestPerWorkflow := false
	if value := query.Get("latestPerWorkflow"); value != "" {
		latestPerWorkflow, err = strconv.ParseBool(value)
		if err != nil {
			return readservice.RunListOptions{}, fmt.Errorf("%w: latestPerWorkflow must be a boolean", readservice.ErrInvalidArgument)
		}
	}
	showNoWork := false
	if value := query.Get("showNoWork"); value != "" {
		showNoWork, err = strconv.ParseBool(value)
		if err != nil {
			return readservice.RunListOptions{}, fmt.Errorf("%w: showNoWork must be a boolean", readservice.ErrInvalidArgument)
		}
	}
	orderByActivity := false
	if value := query.Get("orderByActivity"); value != "" {
		orderByActivity, err = strconv.ParseBool(value)
		if err != nil {
			return readservice.RunListOptions{}, fmt.Errorf("%w: orderByActivity must be a boolean", readservice.ErrInvalidArgument)
		}
	}
	options := readservice.RunListOptions{
		Gaggle:            query.Get("gaggle"),
		Workflow:          query.Get("workflow"),
		Stage:             query.Get("stage"),
		Outcome:           readservice.OutcomeFilter(query.Get("outcome")),
		StagePopulation:   readservice.StagePopulation(query.Get("population")),
		Phase:             readservice.RunPhase(query.Get("phase")),
		Trigger:           readservice.TriggerKind(query.Get("trigger")),
		Since:             since,
		Until:             until,
		Cursor:            query.Get("cursor"),
		LatestPerWorkflow: latestPerWorkflow,
		ShowNoWork:        showNoWork,
		OrderByActivity:   orderByActivity,
	}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return readservice.RunListOptions{}, fmt.Errorf("%w: limit must be an integer", readservice.ErrInvalidArgument)
		}
		options.Limit = limit
	}
	return options, nil
}

// statusClientClosedRequest mirrors nginx's 499: the client aborted the request
// before the server finished. net/http has no named constant for it.
const statusClientClosedRequest = 499

// clientCancelled reports whether err is a client-initiated cancellation of the
// request (the browser navigated away, refreshed, or aborted an in-flight
// fetch) and, if so, reports it quietly with a 499. The read services surface
// cancellation only by returning the request context's Canceled error, so the
// error alone is a reliable signal. Such an error is not a server fault: see
// #1367, where on a busy instance the portal aborts and re-fires reads faster
// than the daemon answers, and logging every cancellation as an error buries
// genuine failures in noise.
func clientCancelled(w http.ResponseWriter, err error) bool {
	if errors.Is(err, context.Canceled) {
		writeError(w, statusClientClosedRequest, "request_cancelled", "the client cancelled the request")
		return true
	}
	return false
}

func writeReadError(w http.ResponseWriter, errorLog *log.Logger, operation string, err error) {
	if clientCancelled(w, err) {
		return
	}
	// The server's own budget expiring is a 503, not a 500. It must be checked
	// before the default: previously context.DeadlineExceeded fell through to
	// "read_error", making a deliberate shed indistinguishable from a fault.
	if budgetExceeded(w, err) {
		return
	}
	switch {
	case errors.Is(err, readservice.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, "invalid_argument", "request parameters are invalid")
	case errors.Is(err, readservice.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "requested run data was not found")
	case errors.Is(err, readservice.ErrArtifactIntegrity):
		errorLog.Printf("%s failed: %v", operation, err)
		writeError(w, http.StatusConflict, "artifact_invalid", "artifact integrity verification failed")
	case errors.Is(err, readservice.ErrTelemetryUnavailable):
		writeError(w, http.StatusServiceUnavailable, "telemetry_unavailable", "telemetry is not enabled")
	default:
		errorLog.Printf("%s failed: %v", operation, err)
		writeError(w, http.StatusInternalServerError, "read_error", "runtime state could not be read")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorEnvelope{Error: APIError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
