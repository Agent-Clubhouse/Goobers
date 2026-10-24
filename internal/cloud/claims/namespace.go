package claims

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
)

// ListNamespace reads one gaggle/provider namespace the way the ledger's
// ListNamespace does: its live holders (live legacy claims included, presented in
// the ledger's unscoped form) and retained released history, newest first. An
// empty gaggle and provider address the legacy namespace alone.
//
// It is the controller's own read, for in-process consumers such as the
// scheduler's demand counters; it carries no pod containment because there is no
// pod principal. Unlike Store.Snapshot and Store.HistorySnapshot it returns every
// query and scan error: a caller deciding from the ABSENCE of a claim must be able
// to tell "no claims" from "the read failed", or a database error would read as
// an unclaimed item.
func (p *Plane) ListNamespace(ctx context.Context, gaggle, provider string) (claimsclient.Listing, error) {
	if (gaggle == "") != (provider == "") {
		return claimsclient.Listing{}, errors.New("claims: gaggle and provider must be given together")
	}
	if gaggle == "" {
		gaggle, provider = legacyNamespace, legacyNamespace
	}
	const namespace = `((gaggle=$2 AND provider=$3) OR (gaggle='` + legacyNamespace + `' AND provider='` + legacyNamespace + `'))`
	const historyNamespace = `((h.gaggle=$2 AND h.provider=$3) OR (h.gaggle='` + legacyNamespace + `' AND h.provider='` + legacyNamespace + `'))`
	entries, err := p.selectEntries(ctx, p.pool, namespace, gaggle, provider)
	if err != nil {
		return claimsclient.Listing{}, err
	}
	history, err := queryHistory(ctx, p.pool, p.instanceID, historyNamespace, historyNewestFirst, gaggle, provider)
	if err != nil {
		return claimsclient.Listing{}, err
	}
	return claimsclient.Listing{Entries: unscoped(entries), History: unscoped(history)}, nil
}

// unscoped presents the scheduler's legacy namespace as the ledger's unscoped
// form, as the API does (wireEntry).
func unscoped(entries []localscheduler.ClaimEntry) []localscheduler.ClaimEntry {
	for i := range entries {
		if entries[i].Gaggle == legacyNamespace && entries[i].Provider == legacyNamespace {
			entries[i].Gaggle, entries[i].Provider = "", ""
		}
	}
	return entries
}
