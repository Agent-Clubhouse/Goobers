package claims

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

// querier is the read surface shared by the pool and a transaction.
type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// List reads the run's claims or one namespace, with released history on
// request. A namespace listing includes live legacy claims, which are
// exclusive against every namespace. The execution listing is the run's own
// snapshot under local claim visibility, the only visibility the cloud has.
func (p *Plane) List(ctx context.Context, request httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
	if request.RunID == "" {
		return httpapi.ClaimListResponse{}, invalidRequest("runId is required")
	}
	switch request.Scope {
	case httpapi.ClaimListScopeRun:
		if request.Execution {
			return p.executionSnapshot(ctx, request)
		}
		return p.listRun(ctx, request)
	case httpapi.ClaimListScopeNamespace:
		if request.Execution {
			return httpapi.ClaimListResponse{}, invalidRequest("execution requires own-run scope and history")
		}
		return p.listNamespace(ctx, request)
	}
	return httpapi.ClaimListResponse{}, invalidRequest("scope must be run or namespace")
}

func (p *Plane) listRun(ctx context.Context, request httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
	entries, err := p.selectEntries(ctx, p.pool, `run_id=$2`, request.RunID)
	if err != nil || !request.IncludeHistory {
		return httpapi.ClaimListResponse{Entries: wireEntries(entries)}, err
	}
	history, err := queryHistory(ctx, p.pool, p.instanceID, `h.run_id=$2`, historyByKey, request.RunID)
	return httpapi.ClaimListResponse{Entries: wireEntries(entries), History: wireEntries(history)}, err
}

func (p *Plane) listNamespace(ctx context.Context, request httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
	if request.Gaggle == "" || request.Provider == "" {
		return httpapi.ClaimListResponse{}, invalidRequest("gaggle and provider are required for a namespace listing")
	}
	if err := p.containPod(ctx, request.PodScoped, request.Gaggle, request.RunID,
		"pod principal may only list the gaggle namespace its own run belongs to"); err != nil {
		return httpapi.ClaimListResponse{}, err
	}
	const namespace = `((gaggle=$2 AND provider=$3) OR (gaggle='` + legacyNamespace + `' AND provider='` + legacyNamespace + `'))`
	const historyNamespace = `((h.gaggle=$2 AND h.provider=$3) OR (h.gaggle='` + legacyNamespace + `' AND h.provider='` + legacyNamespace + `'))`
	entries, err := p.selectEntries(ctx, p.pool, namespace, request.Gaggle, request.Provider)
	if err != nil || !request.IncludeHistory {
		return httpapi.ClaimListResponse{Entries: wireEntries(entries)}, err
	}
	history, err := queryHistory(ctx, p.pool, p.instanceID, historyNamespace, historyNewestFirst, request.Gaggle, request.Provider)
	return httpapi.ClaimListResponse{Entries: wireEntries(entries), History: wireEntries(history)}, err
}

// executionSnapshot answers the trusted execution fence: the run's leases and
// history at one database instant. Shared visibility is refused at config
// admission (RUNTIME004), so every cloud run is local.
func (p *Plane) executionSnapshot(ctx context.Context, request httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
	if !request.IncludeHistory || request.Gaggle != "" || request.Provider != "" {
		return httpapi.ClaimListResponse{}, invalidRequest("execution requires own-run scope and history")
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return httpapi.ClaimListResponse{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	response := httpapi.ClaimListResponse{ClaimVisibility: "local"}
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&response.ObservedAt); err != nil {
		return httpapi.ClaimListResponse{}, err
	}
	entries, err := p.selectEntries(ctx, tx, `run_id=$2`, request.RunID)
	if err != nil {
		return httpapi.ClaimListResponse{}, err
	}
	history, err := queryHistory(ctx, tx, p.instanceID, `h.run_id=$2`, historyByKey, request.RunID)
	if err != nil {
		return httpapi.ClaimListResponse{}, err
	}
	response.Entries, response.History = wireEntries(entries), wireEntries(history)
	return response, tx.Commit(ctx)
}

// ClaimedRuns lists the runs that currently hold at least one claim, and
// ClaimedItems the items one run holds. Unlike the scheduler store's snapshot
// reads, a database failure is returned rather than read as "no claims": the
// terminal hooks decide what to park and which runs to pick up from these.
func (p *Plane) ClaimedRuns(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT run_id FROM control.claim WHERE instance_id=$1 ORDER BY run_id`, p.instanceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ClaimedItems returns the item IDs runID holds.
func (p *Plane) ClaimedItems(ctx context.Context, runID string) ([]string, error) {
	entries, err := p.selectEntries(ctx, p.pool, `run_id=$2`, runID)
	if err != nil {
		return nil, err
	}
	items := make([]string, 0, len(entries))
	for _, entry := range entries {
		items = append(items, entry.ItemID)
	}
	return items, nil
}

func (p *Plane) selectEntries(ctx context.Context, q querier, predicate string, args ...any) ([]localscheduler.ClaimEntry, error) {
	rows, err := q.Query(ctx, claimSelect+`
		FROM control.claim WHERE instance_id=$1 AND `+predicate+`
		ORDER BY gaggle,provider,external_id`,
		append([]any{p.instanceID}, args...)...,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanClaim)
}

func argIndex(n int) string { return strconv.Itoa(n) }

func marshalVerification(observation localscheduler.ClaimVerification) ([]byte, error) {
	return json.Marshal(observation)
}

// containPod confines a pod principal to the gaggle its run was admitted
// into. The run's journal identity is the authority; a run the journal does
// not know is refused.
func (p *Plane) containPod(ctx context.Context, podScoped bool, gaggle, runID, message string) error {
	if !podScoped {
		return nil
	}
	var one int
	err := p.pool.QueryRow(ctx, `
		SELECT 1 FROM journal.run WHERE instance_id=$1 AND run_id=$2 AND gaggle=$3`,
		p.instanceID, runID, gaggle,
	).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpapi.NewInterventionError(http.StatusForbidden, "gaggle_mismatch", message, nil)
	}
	return err
}

// RecordVerification compare-and-swaps a provider observation onto the exact
// lease incarnation the caller read. It is reporting only: it never extends
// the lease, and a stale or older observation answers Ok=false.
func (p *Plane) RecordVerification(ctx context.Context, request httpapi.ClaimVerificationRequest) (httpapi.ClaimResponse, error) {
	for _, value := range []string{request.RunID, request.Gaggle, request.Provider, request.ItemID, request.OwnerRunID} {
		if strings.TrimSpace(value) == "" || len(value) > 1024 {
			return httpapi.ClaimResponse{}, invalidRequest(
				"claim verification requires bounded, nonempty caller, namespace, item and owner identities")
		}
	}
	if err := p.containPod(ctx, request.PodScoped, request.Gaggle, request.RunID,
		"pod principal may only report verification within its own gaggle"); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	expected := localscheduler.ClaimEntry{
		Gaggle: request.Gaggle, Provider: request.Provider, ExternalID: request.ItemID, ItemID: request.ItemID,
		RunID: request.OwnerRunID, ClaimedAt: request.ClaimedAt,
	}
	observation := localscheduler.ClaimVerification{
		State: request.Observation.State, ObservedAt: request.Observation.ObservedAt,
		ProviderRunID: request.Observation.ProviderRunID,
	}
	var updated bool
	err := p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		var err error
		updated, err = p.verify(ctx, tx, epoch, expected, observation)
		return err
	})
	if errors.Is(err, localscheduler.ErrInvalidClaimVerification) {
		return httpapi.ClaimResponse{}, invalidRequest(err.Error())
	}
	return httpapi.ClaimResponse{Ok: updated}, err
}

func (p *Plane) verify(
	ctx context.Context, tx pgx.Tx, epoch int64,
	expected localscheduler.ClaimEntry, observation localscheduler.ClaimVerification,
) (bool, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return false, err
	}
	if err := localscheduler.ValidateClaimVerification(expected, observation, now); err != nil {
		return false, err
	}
	row, err := p.lockRow(ctx, tx, storedKey(expected))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current := row.entry
	if current.RunID != expected.RunID || !current.ClaimedAt.Equal(expected.ClaimedAt) || !row.live ||
		(!current.Verification.ObservedAt.IsZero() && !observation.ObservedAt.After(current.Verification.ObservedAt)) {
		return false, nil
	}
	raw, err := marshalVerification(observation)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE control.claim SET verification=$7, owner_epoch=$6
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
		  AND owner_token=$5 AND owner_epoch<=$6 AND expires_at>clock_timestamp()`,
		p.instanceID, expected.Gaggle, expected.Provider, expected.ExternalID, row.token, epoch, raw,
	)
	if err != nil || tag.RowsAffected() != 1 {
		return false, err
	}
	// The ledger records the observation on the run's history record too.
	return true, recordGrant(ctx, tx, p.instanceID, storedKey(expected), epoch)
}
