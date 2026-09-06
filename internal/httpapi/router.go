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
	// Scopes narrow a POD principal to a subset of the pod-reachable planes
	// (podauth.KnownScopes). Empty means the unscoped pod token, which reaches
	// every pod plane — the posture GOOBERS_POD_TOKEN has always had, and the
	// one __dispatch-exec needs to surrender, resolve credentials and move
	// blobs.
	//
	// A stage SUBPROCESS never holds that token. The dispatcher mints it one
	// scoped bearer per plane (Goobers#3897), so a claims bearer presented to
	// the surrender route is refused HERE, by the authorizer, before any
	// handler's run containment runs. Ignored for human principals, whose
	// authorization is the role ladder.
	Scopes []string
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

// Pod plane scopes, restated from internal/podauth rather than imported:
// podauth depends on this package (it implements Authenticator), so importing
// it back would be a cycle. Pinned against the originals by
// TestPodPlaneScopesMatchPodauth so the restatement cannot drift — the same
// discipline internal/dispatcher applies to the executor's env names.
const (
	ScopeClaims    = "claims"
	ScopeState     = "state"
	ScopeJournal   = "journal"
	ScopeTelemetry = "telemetry"
	ScopeSurrender = "surrender"
	ScopeBlob      = "blob"
	// ScopeConfigDigest admits the config-digest plane: a worker asking the
	// daemon which config tree is currently in force, so it can tell that its
	// own has diverged before a run fails on the mismatch (#4153).
	ScopeConfigDigest = "config-digest"
	ScopeCredential   = "credential"
)

// HasScope reports whether a pod principal may reach the plane named by
// scope. A principal carrying NO scopes is the unscoped pod token and reaches
// every plane; one carrying scopes reaches exactly those.
//
// Fail closed on the empty argument: a call site that cannot name its plane
// has not decided what it is authorizing.
func (p Principal) HasScope(scope string) bool {
	if scope == "" {
		return false
	}
	if len(p.Scopes) == 0 {
		return true
	}
	return slices.Contains(p.Scopes, scope)
}

// podPlanePath reports whether path is one of the fixed pod routes: claims,
// trigger ingest, credential resolve, and the cross-run journal plane's three
// purpose-built questions. Routes with path parameters are matched by their
// dedicated structural helpers below. Handler-level checks bind each request
// to the pod's run or gaggle; everything else stays human-only.
//
// scope names the least-privilege bearer a pod must present for the route
// (Goobers#3897); ok is false for a path that is not a fixed pod route.
func podPlanePath(path string) (scope string, ok bool) {
	switch path {
	case apicontract.ClaimAcquirePath,
		apicontract.ClaimRenewPath,
		apicontract.ClaimReleasePath,
		apicontract.ClaimSettlePath,
		apicontract.ClaimListPath,
		apicontract.ClaimRecoverPath:
		return ScopeClaims, true
	case apicontract.TriggerIngestPath:
		// The trigger plane rides the state bearer: the only pod-side caller
		// is apply-verdict's crowned-lander dispatch, which reaches it
		// through stateclient's own transport and therefore holds exactly
		// the bearer stateclient was handed.
		return ScopeState, true
	case apicontract.CredentialResolvePath:
		return ScopeCredential, true
	case apicontract.JournalRunPhasePath,
		apicontract.JournalConflictTouchesPath,
		apicontract.JournalUnpushedWorkPath,
		apicontract.JournalEscalationCandidatesPath,
		apicontract.JournalBranchOwnershipPath:
		return ScopeJournal, true
	default:
		return "", false
	}
}

// gaggleStatePrefix and gaggleStateInfix are GaggleStateKeyPath split around
// its two wildcards — the literal fragments every scheduler-state request path
// carries. Derived from the contract constant rather than restated, so the two
// cannot drift.
var (
	gaggleStatePrefix = apicontract.GaggleStateKeyPath[:strings.Index(apicontract.GaggleStateKeyPath, "{gaggle}")]
	gaggleStateInfix  = "/state/"
)

// statePlanePath reports whether path is the scheduler-state plane's route
// (decision 005 R3 / finding 002 C2) — the sixth pod-reachable plane. Matched
// structurally, like the journal and surrender planes, because the route
// carries gaggle and key segments; WHICH gaggle the pod may address is
// enforced by the handler and its service, which can see both the path and the
// principal. Handler() has already refused non-clean paths, so segment
// counting is sound.
func statePlanePath(path string) bool {
	rest, ok := strings.CutPrefix(path, gaggleStatePrefix)
	if !ok {
		return false
	}
	gaggle, key, ok := strings.Cut(rest, gaggleStateInfix)
	if !ok {
		return false
	}
	return gaggle != "" && key != "" && !strings.Contains(gaggle, "/") && !strings.Contains(key, "/")
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

// telemetryPlanePath reports whether path is one of the telemetry read routes
// a pod principal may GET (decision 005 R4 / finding 002 C3): the stats and
// errors aggregates, the implementation-outcome evidence derived from the
// same rows, and the defect-nomination aggregate read (Goobers#4001).
// Derived, low-sensitivity data — no raw secret, no other gaggle's
// configuration — and gaggle containment is enforced by the handlers, which
// can see the query string and the principal together
// (registerTelemetryRoutes/podTelemetryGaggle).
//
// TelemetryErrorSignaturesPath is deliberately NOT here, and its absence
// survived Goobers#4001 rather than being overtaken by it. That route serves
// RAW (code, error_class) pairs with an example run, stage and attempt —
// exactly what R4 keeps off the plane. The defect-aggregate route below
// serves the NORMALIZED signature instead
// (telemetryclient.NormalizeErrorSignature), which is what the amended ruling
// admits. Admitting the raw route is still a ruling amendment, not an
// oversight to fix silently.
func telemetryPlanePath(path string) bool {
	switch path {
	case apicontract.TelemetryStatsPath,
		apicontract.TelemetryErrorsPath,
		apicontract.TelemetryImplementationOutcomesPath,
		apicontract.TelemetryDefectAggregatesPath:
		return true
	default:
		return false
	}
}

// RequireRoles authorizes read requests (GET/HEAD) for principals holding
// view or stronger and every other method for operate or stronger. Requests
// without an authenticated principal are denied, so this authorizer must be
// paired with a real Authenticator — under NullAuthenticator every request
// stays anonymous and would be refused.
//
// Pod principals (PodPrincipalIssuer) bypass the role ladder and are confined
// to the machine planes required by a stage: claims, triggers, credentials,
// journal emit/read, blobs, surrender, telemetry, and scheduler state. Their
// token proves "I am run X's stage pod"; handlers then enforce the relevant
// run or gaggle boundary. The credential and blob handlers additionally
// refuse human principals outright.
//
// Since Goobers#3897 the token ALSO proves which planes it may reach. The
// unscoped pod token (GOOBERS_POD_TOKEN, held by __dispatch-exec itself)
// reaches all of them; the per-plane bearers the dispatcher stamps into a
// goobers-CLI stage's environment reach exactly one each. That is what makes
// a stage subprocess unable to surrender its own result even though it holds
// a bearer for the same run — route confinement, checked here, not a naming
// convention.
func RequireRoles() Authorizer {
	return authorizerFunc(func(request *http.Request) error {
		principal, ok := PrincipalFromRequest(request)
		if !ok {
			return errors.New("no authenticated principal")
		}
		if IsPodPrincipal(principal) {
			scope, admitted := podRouteScope(request)
			if !admitted {
				return fmt.Errorf("pod principal %q may only call the claims, trigger, credential, journal, blob, surrender, telemetry-read, scheduler-state, and own-run read planes", principal.Subject)
			}
			if !principal.HasScope(scope) {
				return fmt.Errorf("pod principal %q presents a bearer scoped to %v, which does not admit the %s plane", principal.Subject, principal.Scopes, scope)
			}
			return nil
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

// podRouteScope resolves a request to the pod plane it addresses and the
// scope a bearer must carry for it. admitted=false means the route is not
// pod-reachable at all (or not by this method), which fails closed above.
//
// One function rather than a chain of inline conditions so the route->scope
// mapping is enumerable in one place: a new pod plane that forgets its scope
// does not compile.
func podRouteScope(request *http.Request) (scope string, admitted bool) {
	path, method := request.URL.Path, request.Method
	if scope, ok := podPlanePath(path); ok && method == http.MethodPost {
		return scope, true
	}
	if journalPlanePath(path) && method == http.MethodPost {
		return ScopeJournal, true
	}
	if surrenderPlanePath(path) && method == http.MethodPost {
		return ScopeSurrender, true
	}
	if blobPlanePath(path) && (method == http.MethodGet || method == http.MethodPut) {
		return ScopeBlob, true
	}
	if statePlanePath(path) && (method == http.MethodGet || method == http.MethodPut) {
		return ScopeState, true
	}
	if telemetryPlanePath(path) && method == http.MethodGet {
		return ScopeTelemetry, true
	}
	// The config-digest plane is GET-only and returns one hash. A digest is a
	// hash of configuration, never configuration — the same property that lets
	// gate_pin_missing name digests in the run journal, which is read by more
	// people than the config tree is.
	if path == apicontract.ConfigDigestPath && method == http.MethodGet {
		return ScopeConfigDigest, true
	}
	// Decision 005 R1 option 1: reads of the pod's own run's journal, GET
	// only. A pod may never write through a read route, and the handler still
	// decides WHICH run.
	if runReadPlanePath(path) && method == http.MethodGet {
		return ScopeJournal, true
	}
	return "", false
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
	workflowMutations   WorkflowMutationService
	claims              ClaimService
	triggers            TriggerService
	escalations         EscalationService
	cancels             CancelService
	journal             JournalService
	runJournal          RunJournalService
	credentials         CredentialService
	blobs               blobstore.Store
	surrenders          SurrenderService
	state               StateService
	telemetryDefects    TelemetryDefectAggregateService
	podRunGaggle        func(context.Context, string) (string, error)
	configDigest        func() string
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

// WithConfigDigest registers the daemon's current config-tree digest source
// for the config-digest plane (#4153).
//
// A function, not a value: the daemon's tree moves under it (workflowSource
// syncs it live), and a digest captured once at wiring time would report the
// tree that was in force at startup — which is exactly the stale answer this
// plane exists to expose in someone else.
//
// Unregistered, the route reports that the daemon does not publish a digest
// rather than an empty one: "" would be indistinguishable from a real digest
// that happens to be unknown, and a worker comparing against it would
// conclude it had diverged from everything.
func WithConfigDigest(digest func() string) HandlerOption {
	return func(c *handlerConfig) error {
		if digest == nil {
			return errors.New("http api: config digest source is required")
		}
		c.configDigest = digest
		return nil
	}
}

// WithPodRunGaggle supplies the run-to-gaggle resolution the telemetry read
// plane contains pod principals with (decision 005 R4 / finding 002 C3). A
// pod token proves "I am run X's stage pod" and nothing about which gaggle X
// belongs to; without this seam the handler cannot answer that question, so
// it refuses every pod telemetry read rather than serving an unscoped one.
// Wiring it is therefore what OPENS the plane, not what restricts it.
func WithPodRunGaggle(resolve func(context.Context, string) (string, error)) HandlerOption {
	return func(config *handlerConfig) error {
		if resolve == nil {
			return errors.New("pod run gaggle resolver is required")
		}
		config.podRunGaggle = resolve
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

// WithWorkflowMutations enables the workflow-enable/disable route that
// atomically toggles the scheduler's honor bit for a workflow's non-manual
// triggers. The service is expected to write the change to the workflow's
// on-disk source and drive a hot reload before returning.
func WithWorkflowMutations(service WorkflowMutationService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API workflow mutation service is required")
		}
		config.workflowMutations = service
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
	router.Handle(apicontract.RouteConfigDigest, func(w http.ResponseWriter, request *http.Request) {
		if config.configDigest == nil {
			writeError(w, http.StatusServiceUnavailable, "config_digest_unavailable",
				"this daemon does not publish a config-tree digest")
			return
		}
		digest := config.configDigest()
		if digest == "" {
			writeError(w, http.StatusServiceUnavailable, "config_digest_unavailable",
				"the daemon has not resolved a config-tree digest yet")
			return
		}
		writeJSON(w, http.StatusOK, ConfigDigest{Digest: digest})
	})
	router.Handle(apicontract.RoutePortalConfig, func(w http.ResponseWriter, request *http.Request) {
		portalConfig, err := reader.PortalConfig(request.Context())
		if err != nil {
			errorLog.Printf("portal config read failed: %v", err)
			writeError(w, http.StatusInternalServerError, "read_error", "portal config could not be read")
			return
		}
		portalConfig.Capabilities.RevealRun = config.runRevealer != nil
		portalConfig.Capabilities.WorkflowEnable = config.workflowMutations != nil
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, portalConfig)
	})
	registerTelemetryRoutes(router, reader, config.podRunGaggle, errorLog)
	registerTelemetryDefectAggregateRoute(router, config.telemetryDefects, config.podRunGaggle, errorLog)
	registerRunRoutes(router, reader, errorLog)
	registerInventoryRoutes(router, reader, errorLog)
	registerMutationRoutes(router, config.interventions, config.interventionContext, errorLog)
	registerRunRevealRoute(router, config.runRevealer, errorLog)
	registerWorkflowMutationRoutes(router, config.workflowMutations, errorLog)
	registerWritePlaneRoutes(router, config, errorLog)
	registerJournalPlaneRoutes(router, config, errorLog)
	registerRunJournalPlaneRoutes(router, config, errorLog)
	registerBlobPlaneRoutes(router, config.blobs, errorLog)
	registerSurrenderPlaneRoutes(router, config, errorLog)
	registerStatePlaneRoutes(router, config.state, errorLog)
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
		w.Header().Set(apicontract.DigestHeader, artifact.Metadata.Digest)
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

// ConfigDigest is the config-digest plane's response: the content digest of
// the config tree the daemon currently has in force.
//
// A digest only — never the tree, never a name from it. That is what makes
// the answer safe to hand a worker over the same bearer it already holds: a
// hash tells the asker whether it agrees, and nothing about what it would be
// agreeing to.
type ConfigDigest struct {
	Digest string `json:"digest"`
}
