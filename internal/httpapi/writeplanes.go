package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// writeplanes.go implements the daemon write API's claims, trigger, and HITL
// planes (distributed-state-and-coordination.md §7, DS2/DS3): the versioned
// write surface that lets ledger-touching stages, external trigger sources,
// and escalation resolvers reach instance state without filesystem access to
// the instance root. The daemon stays the only writer (DS1) — these routes
// are transport in front of the same ledger, scheduler, and journal writers
// the local paths use, never a second coordinator.

// ClaimRequest identifies one claim-ledger operation. The key vocabulary is
// the ledger's own (localscheduler.ClaimKey): gaggle + provider + external
// item ID, held by a run under a lease.
type ClaimRequest struct {
	Gaggle   string `json:"gaggle"`
	Provider string `json:"provider"`
	// ItemID is the provider-external work item identity (issue number, PR
	// number) being claimed. Required on acquire, renew, and settle. On
	// release it may be omitted, which means "every claim RunID holds" —
	// narrowed to the gaggle/provider namespace when one is given — the
	// plane's form of the CLI's release-all-for-run (finding 002 C1: the
	// backlog-query --release / issue-close-out / FinalizeTerminal shape).
	ItemID   string `json:"itemId,omitempty"`
	RunID    string `json:"runId"`
	Workflow string `json:"workflow,omitempty"`
	// LeaseSeconds bounds the lease for acquire/renew. Zero takes the
	// instance default; negative is refused (a non-positive lease would be
	// expired at write, silently admitting a second claimant — the #235 rule);
	// values above MaxClaimLeaseSeconds are refused (an effectively-unexpiring
	// lease defeats lease-based liveness).
	LeaseSeconds int `json:"leaseSeconds,omitempty"`
	// Outcome is settle-only: how the run concluded its work on the item
	// (e.g. "completed", "abandoned"), recorded with the settle's journal
	// event. Refused on the other operations.
	Outcome string `json:"outcome,omitempty"`
}

// MaxClaimLeaseSeconds is the ceiling on ClaimRequest.LeaseSeconds: four
// times the daemon's default claim lease (30 minutes — cmd/goobers
// DefaultClaimLease, whose 4× multiple this must track). Every CLI claimant
// is bounded by that default; an API caller gets generous room for a slow
// stage but can never hold an item for years, which would defeat lease-based
// liveness. The cap also keeps the seconds→time.Duration conversion far from
// the ~9.3e9-second overflow that flips a duration negative — over-cap and
// overflow-range values alike are a 400, never a 500.
const MaxClaimLeaseSeconds = 4 * 30 * 60

// ClaimResponse reports the ledger's decision for one claims-plane call.
type ClaimResponse struct {
	// Ok reports whether the operation took effect for the calling run:
	// acquire/renew — the lease is held by runId until ExpiresAt; release and
	// settle are idempotent (a claim no longer held releases as a no-op with
	// Ok=true, mirroring ClaimLedger.Release), so exactly one claimant ever
	// wins while retries stay safe.
	Ok bool `json:"ok"`
	// Holder names the run currently holding the lease when acquire/renew is
	// refused.
	Holder string `json:"holder,omitempty"`
	// ExpiresAt is the lease expiry after a successful acquire/renew.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Released lists the claims a release-all-for-run (release with itemId
	// omitted) actually surrendered, so the caller can mirror each one onto
	// its provider-visible marker. Empty for a single-item release.
	Released []ClaimEntry `json:"released,omitempty"`
}

// ClaimEntry is the wire form of one ledger lease
// (localscheduler.ClaimEntry, restated with identical JSON tags so the two
// round-trip; the daemon converts). Restated rather than imported for the
// same reason internal/dispatcher restates MintedCredential: this package is
// the server, and the ledger package has no business depending on it.
type ClaimEntry struct {
	ItemID     string     `json:"itemId"`
	Gaggle     string     `json:"gaggle,omitempty"`
	Provider   string     `json:"provider,omitempty"`
	ExternalID string     `json:"externalId,omitempty"`
	RunID      string     `json:"runId"`
	Workflow   string     `json:"workflow"`
	ClaimedAt  time.Time  `json:"claimedAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	ReleasedAt *time.Time `json:"releasedAt,omitempty"`
}

// Claim list scopes.
const (
	// ClaimListScopeRun lists every claim RunID currently holds, across every
	// namespace — the ledger's ForRunAll.
	ClaimListScopeRun = "run"
	// ClaimListScopeNamespace lists the gaggle/provider namespace: its current
	// holders (plus the ledger's legacy unscoped entries, which are exclusive
	// against every namespace) and, when IncludeHistory is set, its released
	// history — the input the failure-streak deprioritization reads.
	ClaimListScopeNamespace = "namespace"
)

// ClaimListRequest asks the claims plane for a read of the ledger.
type ClaimListRequest struct {
	Gaggle   string `json:"gaggle,omitempty"`
	Provider string `json:"provider,omitempty"`
	// RunID is the caller's own run: the subject of ClaimListScopeRun and, for
	// a pod principal, the containment key on every scope.
	RunID string `json:"runId"`
	// Scope is ClaimListScopeRun or ClaimListScopeNamespace.
	Scope          string `json:"scope"`
	IncludeHistory bool   `json:"includeHistory,omitempty"`
	// PodScoped is set by the route, never decoded from the body: the caller
	// is a pod principal, so a namespace listing must be confined to the
	// gaggle the caller's run belongs to (the service verifies RunID lives
	// there and refuses otherwise).
	PodScoped bool `json:"-"`
}

// ClaimListResponse is the ledger slice the list route answers with.
type ClaimListResponse struct {
	Entries []ClaimEntry `json:"entries"`
	History []ClaimEntry `json:"history,omitempty"`
}

// ClaimRecoverRequest asks the daemon to run its own stale-claim recovery
// sweep — release every expired lease and every lease whose owning run is
// already terminal — and report what it released.
//
// It carries no item, namespace or lease: the sweep is instance-wide by
// nature, and the ONLY thing the caller supplies is its own identity. This is
// deliberate. A stage pod cannot perform the sweep itself at any fidelity:
// terminality is read from the owning run's journal under the instance root,
// active interventions are in-memory daemon state, and the restart-time
// recovery gate exists precisely so a sweep never races the renewal pass. The
// route therefore delegates the WHOLE decision to the daemon rather than
// exposing a lock a pod could take over a filesystem it does not have.
type ClaimRecoverRequest struct {
	// RunID is the caller's own run — the plane's containment key, checked
	// against a pod principal's subject exactly as the mutations are.
	RunID string `json:"runId"`
	// PodScoped is set by the route, never decoded from the body.
	PodScoped bool `json:"-"`
}

// ClaimRecoverResponse reports the leases the sweep released. Empty is the
// ordinary answer: a healthy ledger has nothing stale in it.
type ClaimRecoverResponse struct {
	Released []ClaimEntry `json:"released,omitempty"`
}

// ClaimService is the daemon-side claims plane. Implementations wrap the
// existing claim ledger under its existing cross-process atomicity (the flock
// plus fresh-open discipline the CLI claimants use); the ledger file remains
// the store (DS3 — API contract first, store swappable behind it).
type ClaimService interface {
	Acquire(ctx context.Context, request ClaimRequest) (ClaimResponse, error)
	Renew(ctx context.Context, request ClaimRequest) (ClaimResponse, error)
	// Release surrenders one claim, or — with ItemID empty — every claim the
	// run holds in the (optional) namespace, reporting them in Released.
	Release(ctx context.Context, request ClaimRequest) (ClaimResponse, error)
	Settle(ctx context.Context, request ClaimRequest) (ClaimResponse, error)
	// List reads the ledger for the caller's run or namespace.
	List(ctx context.Context, request ClaimListRequest) (ClaimListResponse, error)
	// Recover runs the daemon's own stale-claim sweep and reports the
	// released leases (Goobers#4016).
	Recover(ctx context.Context, request ClaimRecoverRequest) (ClaimRecoverResponse, error)
}

// TriggerRequest asks the daemon to mint one workflow run. RequestID is the
// caller's delivery identity: redelivering the same RequestID never mints a
// second run (the webhook handler's bounded in-memory dedupe, applied to the
// generic trigger plane — daemon-local is sound under DS1).
type TriggerRequest struct {
	Gaggle    string `json:"gaggle,omitempty"`
	Workflow  string `json:"workflow"`
	RequestID string `json:"requestId,omitempty"`
	// SourceRun names the run whose newly-published durable state is the
	// reason for this trigger. Non-empty makes it a PRIORITY re-tick
	// (Scheduler.TriggerPriority) rather than an ordinary mint — the plane's
	// form of apply-verdict's crowned-lander file drop
	// (writePriorityTriggerRequest), which a stage pod has no scheduler
	// directory to write. It is an output-driven signal, not a bypass: normal
	// readiness admission still applies.
	SourceRun string `json:"sourceRun,omitempty"`
	// PodScoped and PodRunID are set by the route, never decoded from the
	// body: the caller is a pod principal, so the trigger must name the
	// gaggle the caller's run belongs to (the service verifies it) and a
	// priority re-tick must name the caller's own run as its source.
	PodScoped bool   `json:"-"`
	PodRunID  string `json:"-"`
}

// MaxTriggerRequestIDBytes caps the caller-supplied delivery identity — the
// same 256-byte bound the webhook handler puts on GitHub delivery ids
// (internal/webhook maxHeaderBytes). The dedupe record is bounded by entry
// count, not bytes, so unbounded ids would let 10k retained entries grow to
// gigabytes.
const MaxTriggerRequestIDBytes = 256

// TriggerResponse reports the minted run, or the original run when RequestID
// deduplicated a redelivery (the run id may still be empty when the
// deduplicated delivery is concurrent with the winning delivery's mint).
type TriggerResponse struct {
	RunID string `json:"runId,omitempty"`
	// Duplicate marks a response answered from the dedupe record rather than
	// a fresh mint.
	Duplicate bool `json:"duplicate,omitempty"`
}

// TriggerService ingests external triggers through the same
// validate/dedupe/mint path the daemon's pending-triggers sweep uses.
type TriggerService interface {
	Trigger(ctx context.Context, request TriggerRequest) (TriggerResponse, error)
}

// Escalation resolutions.
const (
	EscalationResolutionApprove  = "approve"
	EscalationResolutionDeny     = "deny"
	EscalationResolutionRedirect = "redirect"
)

// EscalationResolutionRequest resolves an escalated run: approve resumes it
// through the escalated gate's pass branch, redirect resumes it through a
// chosen decision branch, deny records the escalation as resolved-denied and
// leaves the run terminal. Every resolution is journaled as the resolution
// event by the run's own journal writer.
type EscalationResolutionRequest struct {
	RunID          string `json:"-"`
	IdempotencyKey string `json:"-"`
	Actor          string `json:"actor,omitempty"`
	Resolution     string `json:"resolution"`
	// Gate names the escalated gate for approve/redirect.
	Gate string `json:"gate,omitempty"`
	// Decision is the branch decision for redirect (defaults to "pass" for
	// approve).
	Decision  string `json:"decision,omitempty"`
	Rationale string `json:"rationale,omitempty"`
}

// EscalationService accepts a resolution for an escalated run. The two
// contexts mirror InterventionService: admission bounds acceptance, execution
// binds the resumed run to the daemon lifecycle.
type EscalationService interface {
	AcceptResolve(admission, execution context.Context, input EscalationResolutionRequest) (InterventionResult, error)
}

// Cancel dispositions, the wire form of the daemon's existing cancel-response
// codes: the run was cancelled and finalized aborted, it finished on its own
// before the cancel landed, or this daemon is not executing it.
const (
	CancelCodeAborted    = "aborted"
	CancelCodeTerminal   = "already_terminal"
	CancelCodeNotRunning = "not_running"
)

// CancelRunRequest asks the daemon to stop a run it is actively executing
// (#3807). Workflow and Gaggle are the run's own identity, read from its
// journal by the caller; the daemon uses them to resolve the owning Runner and
// release the scheduler's concurrency slot, exactly as the file-drop seam does.
type CancelRunRequest struct {
	RunID    string `json:"-"`
	Workflow string `json:"workflow,omitempty"`
	Gaggle   string `json:"gaggle,omitempty"`
	Actor    string `json:"actor,omitempty"`
}

// CancelRunResult reports the cancel disposition. A refusal the operator can
// act on (already terminal, not running under this daemon) is a 200 carrying a
// Code rather than an HTTP error: the request was well-formed and the daemon
// answered it, and the CLI maps the code to its own exit code the same way the
// local file-drop path does.
type CancelRunResult struct {
	Phase string `json:"phase,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// CancelService cancels one live run through the Runner that owns it.
type CancelService interface {
	Cancel(ctx context.Context, input CancelRunRequest) (CancelRunResult, error)
}

// WithClaimService enables the claims-plane routes.
func WithClaimService(claims ClaimService) HandlerOption {
	return func(config *handlerConfig) error {
		if claims == nil {
			return errors.New("http API claim service is required")
		}
		config.claims = claims
		return nil
	}
}

// WithTriggerService enables the trigger-plane route.
func WithTriggerService(triggers TriggerService) HandlerOption {
	return func(config *handlerConfig) error {
		if triggers == nil {
			return errors.New("http API trigger service is required")
		}
		config.triggers = triggers
		return nil
	}
}

// WithEscalationService enables the HITL escalation-resolution route.
func WithEscalationService(escalations EscalationService) HandlerOption {
	return func(config *handlerConfig) error {
		if escalations == nil {
			return errors.New("http API escalation service is required")
		}
		config.escalations = escalations
		return nil
	}
}

// WithCancelService enables the run-control cancel route (#3807).
func WithCancelService(cancels CancelService) HandlerOption {
	return func(config *handlerConfig) error {
		if cancels == nil {
			return errors.New("http API cancel service is required")
		}
		config.cancels = cancels
		return nil
	}
}

func registerWritePlaneRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	registerClaimRoute(router, apicontract.RouteClaimAcquire, config.claims, errorLog, "acquire claim",
		func(ctx context.Context, claims ClaimService, request ClaimRequest) (ClaimResponse, error) {
			return claims.Acquire(ctx, request)
		})
	registerClaimRoute(router, apicontract.RouteClaimRenew, config.claims, errorLog, "renew claim",
		func(ctx context.Context, claims ClaimService, request ClaimRequest) (ClaimResponse, error) {
			return claims.Renew(ctx, request)
		})
	registerClaimRoute(router, apicontract.RouteClaimRelease, config.claims, errorLog, "release claim",
		func(ctx context.Context, claims ClaimService, request ClaimRequest) (ClaimResponse, error) {
			return claims.Release(ctx, request)
		})
	registerClaimRoute(router, apicontract.RouteClaimSettle, config.claims, errorLog, "settle claim",
		func(ctx context.Context, claims ClaimService, request ClaimRequest) (ClaimResponse, error) {
			return claims.Settle(ctx, request)
		})
	registerClaimListRoute(router, config.claims, errorLog)
	registerClaimRecoverRoute(router, config.claims, errorLog)
	registerTriggerRoute(router, config.triggers, errorLog)
	registerEscalationRoute(router, config.escalations, config.interventionContext, errorLog)
	registerCancelRoute(router, config.cancels, errorLog)
	registerCredentialRoute(router, config.credentials, errorLog)
}

func registerClaimRoute(
	router *Router,
	routeID apicontract.RouteID,
	claims ClaimService,
	errorLog *log.Logger,
	operation string,
	call func(context.Context, ClaimService, ClaimRequest) (ClaimResponse, error),
) {
	settle := routeID == apicontract.RouteClaimSettle
	release := routeID == apicontract.RouteClaimRelease
	router.Handle(routeID, func(w http.ResponseWriter, request *http.Request) {
		if claims == nil {
			writeError(w, http.StatusServiceUnavailable, "claims_unavailable", "the claims plane is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input ClaimRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		switch {
		case input.RunID == "":
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "gaggle, provider, itemId, and runId are required")
			return
		case input.ItemID == "" && release:
			// Release-all-for-run: the namespace is an optional narrowing, but
			// half a namespace is neither "all" nor "this namespace".
			if (input.Gaggle == "") != (input.Provider == "") {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "gaggle and provider must be given together for a release of every claim the run holds")
				return
			}
		case input.Gaggle == "" || input.Provider == "" || input.ItemID == "":
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "gaggle, provider, itemId, and runId are required")
			return
		}
		if input.LeaseSeconds < 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "leaseSeconds must not be negative")
			return
		}
		if input.LeaseSeconds > MaxClaimLeaseSeconds {
			writeError(w, http.StatusBadRequest, "invalid_lease",
				fmt.Sprintf("leaseSeconds must not exceed %d", MaxClaimLeaseSeconds))
			return
		}
		if input.Outcome != "" && !settle {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "outcome is only valid for settle")
			return
		}
		// Per-run containment: a pod token proves "I am run X's stage pod",
		// which authorizes ledger operations for run X and no other. The
		// authorizer already confined pods to this plane; the body-level run
		// binding has to happen here, where the body is visible.
		if principal, ok := PrincipalFromRequest(request); ok && IsPodPrincipal(principal) {
			if principal.Subject != podPrincipalSubject(input.RunID) {
				writeError(w, http.StatusForbidden, "run_mismatch", "pod principal may only operate on its own run's claims")
				return
			}
		}
		response, err := call(request.Context(), claims, input)
		if err != nil {
			writePlaneError(w, errorLog, operation, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
}

// podPrincipalSubject is the Principal.Subject a pod token for runID carries.
func podPrincipalSubject(runID string) string { return "run:" + runID }

// registerClaimListRoute serves the claims plane's read. The same containment
// as the mutations (a pod principal names only its own run) plus one more:
// a pod's namespace listing is flagged PodScoped so the service confines it
// to the gaggle the run belongs to — a stage pod has no business reading
// another gaggle's ledger, even read-only.
func registerClaimListRoute(router *Router, claims ClaimService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteClaimList, func(w http.ResponseWriter, request *http.Request) {
		if claims == nil {
			writeError(w, http.StatusServiceUnavailable, "claims_unavailable", "the claims plane is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input ClaimListRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if input.RunID == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "runId is required")
			return
		}
		switch input.Scope {
		case ClaimListScopeRun:
		case ClaimListScopeNamespace:
			if input.Gaggle == "" || input.Provider == "" {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "gaggle and provider are required for a namespace listing")
				return
			}
		default:
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "scope must be run or namespace")
			return
		}
		if principal, ok := PrincipalFromRequest(request); ok && IsPodPrincipal(principal) {
			if principal.Subject != podPrincipalSubject(input.RunID) {
				writeError(w, http.StatusForbidden, "run_mismatch", "pod principal may only list its own run's claims")
				return
			}
			input.PodScoped = true
		}
		response, err := claims.List(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, "list claims", err)
			return
		}
		if response.Entries == nil {
			response.Entries = []ClaimEntry{}
		}
		writeJSON(w, http.StatusOK, response)
	})
}

// registerClaimRecoverRoute serves the claims plane's stale-claim sweep. The
// containment is the mutations' containment: a pod principal may only ask for
// a sweep as its own run. That is an identity check, not an authorization
// narrowing — the sweep itself is instance-wide because staleness is — and it
// is what keeps the plane's audit trail attributable.
func registerClaimRecoverRoute(router *Router, claims ClaimService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteClaimRecover, func(w http.ResponseWriter, request *http.Request) {
		if claims == nil {
			writeError(w, http.StatusServiceUnavailable, "claims_unavailable", "the claims plane is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input ClaimRecoverRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if strings.TrimSpace(input.RunID) == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "runId is required")
			return
		}
		if principal, ok := PrincipalFromRequest(request); ok && IsPodPrincipal(principal) {
			if principal.Subject != podPrincipalSubject(input.RunID) {
				writeError(w, http.StatusForbidden, "run_mismatch", "pod principal may only request claim recovery as its own run")
				return
			}
			input.PodScoped = true
		}
		response, err := claims.Recover(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, "recover claims", err)
			return
		}
		if response.Released == nil {
			response.Released = []ClaimEntry{}
		}
		writeJSON(w, http.StatusOK, response)
	})
}

func registerTriggerRoute(router *Router, triggers TriggerService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteTriggerIngest, func(w http.ResponseWriter, request *http.Request) {
		if triggers == nil {
			writeError(w, http.StatusServiceUnavailable, "triggers_unavailable", "the trigger plane is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input TriggerRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if strings.TrimSpace(input.Workflow) == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "workflow is required")
			return
		}
		if len(input.RequestID) > MaxTriggerRequestIDBytes {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				fmt.Sprintf("requestId must be no longer than %d bytes", MaxTriggerRequestIDBytes))
			return
		}
		// Pod containment (decision 005 ruling R3): a pod token proves "I am
		// run X's stage pod". That authorizes minting a run in the gaggle X
		// belongs to — apply-verdict's crowned-lander priority dispatch — and
		// nothing wider. The gaggle must be named explicitly (an unscoped
		// trigger would let the daemon's ambiguity resolution pick a workflow
		// in some other gaggle), the run the priority re-tick is attributed to
		// must be the caller's own, and the service independently verifies
		// that the run really does live in that gaggle. Fail closed at every
		// step.
		if principal, ok := PrincipalFromRequest(request); ok && IsPodPrincipal(principal) {
			runID, named := podPrincipalRunID(principal)
			if !named {
				writeError(w, http.StatusForbidden, "run_mismatch", "pod principal does not name a run")
				return
			}
			if strings.TrimSpace(input.Gaggle) == "" {
				writeError(w, http.StatusForbidden, "gaggle_required",
					"pod principal must name the gaggle its own run belongs to")
				return
			}
			if source := strings.TrimSpace(input.SourceRun); source != "" && source != runID {
				writeError(w, http.StatusForbidden, "run_mismatch",
					"pod principal may only request a priority trigger for its own run")
				return
			}
			input.PodScoped = true
			input.PodRunID = runID
		}
		response, err := triggers.Trigger(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, "ingest trigger", err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
}

func registerEscalationRoute(router *Router, escalations EscalationService, lifecycle context.Context, errorLog *log.Logger) {
	router.Handle(apicontract.RouteResolveEscalation, func(w http.ResponseWriter, request *http.Request) {
		if escalations == nil {
			writeError(w, http.StatusServiceUnavailable, "escalations_unavailable", "escalation resolution is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		key, ok := requireIdempotencyKey(w, request)
		if !ok {
			return
		}
		var input EscalationResolutionRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		input.RunID = request.PathValue("run")
		input.IdempotencyKey = key
		switch input.Resolution {
		case EscalationResolutionApprove, EscalationResolutionDeny, EscalationResolutionRedirect:
		default:
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "resolution must be approve, deny, or redirect")
			return
		}
		if principal, ok := PrincipalFromRequest(request); ok {
			input.Actor = principal.Subject
		}
		if strings.TrimSpace(input.Actor) == "" {
			writeError(w, http.StatusBadRequest, "actor_required", "actor is required")
			return
		}
		result, err := escalations.AcceptResolve(request.Context(), lifecycle, input)
		if err != nil {
			writePlaneError(w, errorLog, "resolve escalation", err)
			return
		}
		if result.JournalSeq == 0 {
			errorLog.Printf("resolve escalation returned no journal position")
			writeError(w, http.StatusInternalServerError, "escalation_failed", "escalation resolution returned no journal position")
			return
		}
		w.Header().Set(HeaderSourceApplied, fmt.Sprintf("%s:%d", input.RunID, result.JournalSeq))
		writeJSON(w, http.StatusOK, result)
	})
}

// registerCancelRoute serves `run cancel`/`run abort` over the API (#3807).
// The cancel itself stays the daemon's: the service resolves the Runner that
// owns the run and calls the same CancelRun the pending-cancels sweep calls,
// so a remote cancel and a local one are one code path with two ways in.
func registerCancelRoute(router *Router, cancels CancelService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteCancelRun, func(w http.ResponseWriter, request *http.Request) {
		if cancels == nil {
			writeError(w, http.StatusServiceUnavailable, "cancel_unavailable", "run cancellation is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		if _, ok := requireIdempotencyKey(w, request); !ok {
			return
		}
		var input CancelRunRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		input.RunID = request.PathValue("run")
		if strings.TrimSpace(input.RunID) == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "run is required")
			return
		}
		if principal, ok := PrincipalFromRequest(request); ok {
			input.Actor = principal.Subject
		}
		if strings.TrimSpace(input.Actor) == "" {
			writeError(w, http.StatusBadRequest, "actor_required", "actor is required")
			return
		}
		result, err := cancels.Cancel(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, "cancel run", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

// decodeWriteRequest decodes exactly one JSON object into target with unknown
// fields refused, bounded by the shared mutation body cap.
func decodeWriteRequest(request *http.Request, target any) error {
	return decodeWriteRequestBounded(request, target, maxInterventionBody)
}

// decodeWriteRequestBounded is decodeWriteRequest with an explicit body cap —
// the journal plane's batches carry artifact bytes inline and get a larger
// bound than operator mutations.
func decodeWriteRequestBounded(request *http.Request, target any, maxBytes int64) error {
	defer func() { _ = request.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("JSON request body is required")
		}
		return fmt.Errorf("invalid JSON request body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON object")
		}
		return fmt.Errorf("invalid JSON request body: %w", err)
	}
	return nil
}

// writePlaneError maps a write-plane service failure onto the shared error
// envelope: typed refusals (InterventionError carries the status/code/message
// triple every plane shares) pass through, everything else is a 500 that
// never leaks internals.
func writePlaneError(w http.ResponseWriter, errorLog *log.Logger, operation string, err error) {
	if budgetExceeded(w, err) {
		return
	}
	var planeErr *InterventionError
	if errors.As(err, &planeErr) {
		status := planeErr.Status
		if status < 400 || status > 599 {
			status = http.StatusInternalServerError
		}
		if status >= http.StatusInternalServerError {
			errorLog.Printf("%s failed: %v", operation, err)
		}
		code := planeErr.Code
		if code == "" {
			code = "write_failed"
		}
		message := planeErr.Message
		if message == "" {
			message = operation + " failed"
		}
		writeError(w, status, code, message)
		return
	}
	errorLog.Printf("%s failed: %v", operation, err)
	writeError(w, http.StatusInternalServerError, "write_failed", operation+" failed")
}
