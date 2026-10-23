package claims

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

// settleOutcomes is settle's closed outcome vocabulary.
var settleOutcomes = map[string]bool{"completed": true, "abandoned": true}

// Release gives the item back. Releasing a claim the run does not hold is a
// no-op answering Ok=true; with ItemID empty it releases every claim the run
// holds except publication locks. Provider-cleanup release needs server-side
// marker custody the cloud does not have yet.
func (p *Plane) Release(ctx context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	if request.ProviderCleanup {
		return httpapi.ClaimResponse{}, errProviderCleanupUnavailable()
	}
	if request.ItemID == "" {
		return p.releaseAllForRun(ctx, request)
	}
	return p.releaseOne(ctx, request, request.Reason)
}

// Settle is the exactly-once terminal release: the outcome is the release
// disposition, and a retried settle after the release is a no-op.
func (p *Plane) Settle(ctx context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	outcome := strings.TrimSpace(request.Outcome)
	if outcome == "" {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, "outcome_required", "settle requires an outcome", nil)
	}
	if !settleOutcomes[outcome] {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, "invalid_outcome",
			"settle outcome must be completed or abandoned", nil)
	}
	reason := request.Reason
	reason.Disposition = claimrelease.Disposition(outcome)
	if reason.Code == "" {
		reason.Code = "settled"
	}
	return p.releaseOne(ctx, request, reason)
}

func (p *Plane) releaseOne(ctx context.Context, request httpapi.ClaimRequest, reason claimrelease.Reason) (httpapi.ClaimResponse, error) {
	if err := requireKey(request); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	reason = reason.Normalize()
	if localscheduler.IsPublicationClaimKey(request.ItemID) && reason.Code != localscheduler.PublicationLockReleaseCode {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusConflict, "publication_custody",
			"publication custody ends only by verified release ("+localscheduler.PublicationLockReleaseCode+") or expiry", nil)
	}
	key := localscheduler.ClaimKey{Gaggle: request.Gaggle, Provider: request.Provider, ExternalID: request.ItemID}
	err := p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		_, err := p.moveToHistory(ctx, tx, epoch, key, request.RunID, "", false, reason)
		return err
	})
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	return httpapi.ClaimResponse{Ok: true}, nil
}

// releaseAllForRun releases the run's non-publication claims, narrowed to one
// namespace when gaggle and provider are given, and reports each release so
// the caller can mirror it onto the provider marker.
func (p *Plane) releaseAllForRun(ctx context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	if strings.TrimSpace(request.RunID) == "" {
		return httpapi.ClaimResponse{}, invalidRequest("runId is required")
	}
	if (request.Gaggle == "") != (request.Provider == "") {
		return httpapi.ClaimResponse{}, invalidRequest(
			"gaggle and provider must be given together for a release of every claim the run holds")
	}
	reason := request.Reason.Normalize()
	var released []httpapi.ClaimEntry
	err := p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		released = nil
		entries, err := p.runEntries(ctx, tx, request.RunID)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsPublicationClaim() ||
				(request.Gaggle != "" && (entry.Gaggle != request.Gaggle || entry.Provider != request.Provider)) {
				continue
			}
			moved, err := p.moveToHistory(ctx, tx, epoch, storedKey(entry), request.RunID, "", false, reason)
			if err != nil {
				return err
			}
			if moved {
				released = append(released, wireEntry(entry))
			}
		}
		return nil
	})
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	return httpapi.ClaimResponse{Ok: true, Released: released}, nil
}

// moveToHistory releases the run's row for key and completes its history
// record, adopting a predecessor epoch's row. token, when set, pins one
// incarnation and requireExpired rechecks expiry under the fence (recovery
// decides outside it); otherwise the run's current incarnation is released.
func (p *Plane) moveToHistory(
	ctx context.Context, tx pgx.Tx, epoch int64, key localscheduler.ClaimKey,
	runID, token string, requireExpired bool, reason claimrelease.Reason,
) (bool, error) {
	return releaseToHistory(ctx, tx, p.instanceID, epoch, key, runID, token, requireExpired, reason)
}

// runEntries reads the run's active claims inside the transaction.
func (p *Plane) runEntries(ctx context.Context, q pgx.Tx, runID string) ([]localscheduler.ClaimEntry, error) {
	rows, err := q.Query(ctx, claimSelect+`
		FROM control.claim WHERE instance_id=$1 AND run_id=$2
		ORDER BY gaggle,provider,external_id`,
		p.instanceID, runID,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanClaim)
}

// storedKey is an entry's persisted key, including the legacy namespace.
func storedKey(entry localscheduler.ClaimEntry) localscheduler.ClaimKey {
	return localscheduler.ClaimKey{Gaggle: entry.Gaggle, Provider: entry.Provider, ExternalID: entry.ExternalID}
}

// wireEntry converts an entry for the API, presenting the scheduler's legacy
// namespace as the ledger's unscoped form.
func wireEntry(entry localscheduler.ClaimEntry) httpapi.ClaimEntry {
	if entry.Gaggle == legacyNamespace && entry.Provider == legacyNamespace {
		entry.Gaggle, entry.Provider = "", ""
	}
	return planes.ClaimEntryWire(entry)
}

func wireEntries(entries []localscheduler.ClaimEntry) []httpapi.ClaimEntry {
	if len(entries) == 0 {
		return nil
	}
	wire := make([]httpapi.ClaimEntry, 0, len(entries))
	for _, entry := range entries {
		wire = append(wire, wireEntry(entry))
	}
	return wire
}
