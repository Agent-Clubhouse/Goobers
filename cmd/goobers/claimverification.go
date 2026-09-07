package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

func (s *daemonClaimService) RecordVerification(_ context.Context, request httpapi.ClaimVerificationRequest) (httpapi.ClaimResponse, error) {
	for _, value := range []string{request.RunID, request.Gaggle, request.Provider, request.ItemID, request.OwnerRunID} {
		if strings.TrimSpace(value) == "" || len(value) > 1024 {
			return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, "claim verification requires bounded, nonempty caller, namespace, item and owner identities", nil)
		}
	}
	if request.PodScoped && !s.runBelongsToGaggle(request.Gaggle, request.RunID) {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusForbidden, "gaggle_mismatch", "pod principal may only report verification within its own gaggle", nil)
	}
	var response httpapi.ClaimResponse
	err := s.withLedger("api.claims.verify", httpapi.ClaimRequest{Gaggle: request.Gaggle, RunID: request.RunID}, func(ledger *localscheduler.ClaimLedger) error {
		entry := localscheduler.ClaimEntry{
			Gaggle: request.Gaggle, Provider: request.Provider, ExternalID: request.ItemID,
			RunID: request.OwnerRunID, ClaimedAt: request.ClaimedAt,
		}
		observation := localscheduler.ClaimVerification{
			State: request.Observation.State, ObservedAt: request.Observation.ObservedAt,
			ProviderRunID: request.Observation.ProviderRunID,
		}
		var err error
		response.Ok, err = ledger.RecordClaimVerification(entry, observation)
		if errors.Is(err, localscheduler.ErrInvalidClaimVerification) {
			return httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), nil)
		}
		return err
	})
	return response, err
}
