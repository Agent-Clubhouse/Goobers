package main

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// claimHistoryForRun returns the durable claim set a run proved ownership of,
// including runs whose active ledger entries have already been released.
func claimHistoryForRun(layout instance.Layout, runID string, fallbackProvider apiv1.Provider) ([]localscheduler.ClaimEntry, error) {
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		return nil, err
	}
	durable := ledger.HistoryForRun(runID)
	claims := make(map[string]localscheduler.ClaimEntry, len(durable))
	for _, entry := range durable {
		key := entry.Gaggle + "\x00" + entry.Provider + "\x00" + entry.ExternalID
		claims[key] = entry
	}

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		if len(durable) > 0 {
			return durable, nil
		}
		return nil, err
	}
	for _, event := range events {
		if event.Type != journal.EventClaimAcquired || event.RunID != runID {
			continue
		}
		itemID := strings.TrimSpace(event.Name)
		if itemID == "" {
			return nil, errors.New("claim acquisition event has no item identity")
		}
		externalID, _ := event.Runner["claimExternalId"].(string)
		if externalID == "" {
			externalID = itemID
		}
		provider, _ := event.Runner["claimProvider"].(string)
		if provider == "" && event.Gaggle != "" {
			provider = string(fallbackProvider)
		}
		entry := localscheduler.ClaimEntry{
			ItemID:     itemID,
			Gaggle:     event.Gaggle,
			Provider:   provider,
			ExternalID: externalID,
			RunID:      runID,
			Workflow:   event.Workflow,
		}
		key := entry.Gaggle + "\x00" + entry.Provider + "\x00" + entry.ExternalID
		claims[key] = entry
	}

	result := make([]localscheduler.ClaimEntry, 0, len(claims))
	for _, entry := range claims {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Gaggle != result[j].Gaggle {
			return result[i].Gaggle < result[j].Gaggle
		}
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		return result[i].ExternalID < result[j].ExternalID
	})
	return result, nil
}
