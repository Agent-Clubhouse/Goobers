package httpapi

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// ClaimVerificationRequest binds a provider observation to the exact ledger
// lease read before the provider request. RunID is the caller, OwnerRunID the
// observed lease holder; reconciliation can observe another run in its gaggle.
type ClaimVerificationRequest struct {
	RunID       string            `json:"runId"`
	Gaggle      string            `json:"gaggle"`
	Provider    string            `json:"provider"`
	ItemID      string            `json:"itemId"`
	OwnerRunID  string            `json:"ownerRunId"`
	ClaimedAt   time.Time         `json:"claimedAt"`
	Observation ClaimVerification `json:"observation"`
	PodScoped   bool              `json:"-"`
}

// ClaimVerificationService is the reporting-only extension of the claims plane.
type ClaimVerificationService interface {
	RecordVerification(context.Context, ClaimVerificationRequest) (ClaimResponse, error)
}

func registerClaimVerificationRoute(router *Router, claims ClaimService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteClaimVerify, func(w http.ResponseWriter, request *http.Request) {
		service, ok := claims.(ClaimVerificationService)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "claims_unavailable", "claim verification is unavailable")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input ClaimVerificationRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if principal, ok := PrincipalFromRequest(request); ok && IsPodPrincipal(principal) {
			if principal.Subject != podPrincipalSubject(input.RunID) {
				writeError(w, http.StatusForbidden, "run_mismatch", "pod principal may only report as its own run")
				return
			}
			input.PodScoped = true
		}
		response, err := service.RecordVerification(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, "record claim verification", err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
}
