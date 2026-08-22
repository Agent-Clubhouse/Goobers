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
	// number) being claimed.
	ItemID   string `json:"itemId"`
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
}

// ClaimService is the daemon-side claims plane. Implementations wrap the
// existing claim ledger under its existing cross-process atomicity (the flock
// plus fresh-open discipline the CLI claimants use); the ledger file remains
// the store (DS3 — API contract first, store swappable behind it).
type ClaimService interface {
	Acquire(ctx context.Context, request ClaimRequest) (ClaimResponse, error)
	Renew(ctx context.Context, request ClaimRequest) (ClaimResponse, error)
	Release(ctx context.Context, request ClaimRequest) (ClaimResponse, error)
	Settle(ctx context.Context, request ClaimRequest) (ClaimResponse, error)
}

// TriggerRequest asks the daemon to mint one workflow run. RequestID is the
// caller's delivery identity: redelivering the same RequestID never mints a
// second run (the webhook handler's bounded in-memory dedupe, applied to the
// generic trigger plane — daemon-local is sound under DS1).
type TriggerRequest struct {
	Gaggle    string `json:"gaggle,omitempty"`
	Workflow  string `json:"workflow"`
	RequestID string `json:"requestId,omitempty"`
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
	registerTriggerRoute(router, config.triggers, errorLog)
	registerEscalationRoute(router, config.escalations, config.interventionContext, errorLog)
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
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if input.Gaggle == "" || input.Provider == "" || input.ItemID == "" || input.RunID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "gaggle, provider, itemId, and runId are required")
			return
		}
		if input.LeaseSeconds < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "leaseSeconds must not be negative")
			return
		}
		if input.LeaseSeconds > MaxClaimLeaseSeconds {
			writeError(w, http.StatusBadRequest, "invalid_lease",
				fmt.Sprintf("leaseSeconds must not exceed %d", MaxClaimLeaseSeconds))
			return
		}
		if input.Outcome != "" && !settle {
			writeError(w, http.StatusBadRequest, "invalid_request", "outcome is only valid for settle")
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
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if strings.TrimSpace(input.Workflow) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "workflow is required")
			return
		}
		if len(input.RequestID) > MaxTriggerRequestIDBytes {
			writeError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("requestId must be no longer than %d bytes", MaxTriggerRequestIDBytes))
			return
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
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		input.RunID = request.PathValue("run")
		input.IdempotencyKey = key
		switch input.Resolution {
		case EscalationResolutionApprove, EscalationResolutionDeny, EscalationResolutionRedirect:
		default:
			writeError(w, http.StatusBadRequest, "invalid_request", "resolution must be approve, deny, or redirect")
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

// decodeWriteRequest decodes exactly one JSON object into target with unknown
// fields refused, bounded by the shared mutation body cap.
func decodeWriteRequest(request *http.Request, target any) error {
	defer func() { _ = request.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxInterventionBody))
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
