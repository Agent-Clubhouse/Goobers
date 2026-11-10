package runtimewiring

import (
	"context"
	"log/slog"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// IntakeRecord is one shadow duplicate/dependency reading of a claimed item.
// It carries identities and a verdict, never issue text.
type IntakeRecord struct {
	ItemID     string
	Kind       decisiongate.IntakeKind
	Related    string
	Basis      decisiongate.IntakeBasis
	Candidates int
}

// maxIntakePerClaim bounds the items read per claim.
const maxIntakePerClaim = 3

// IntakeShadow reads each claimed item against the repository's open items
// when the instance opted in (decisionGate shadow plus intakeDetection). It is
// advisory: it returns nil when disabled or unusable, never an error, and its
// readings must not change what is claimed.
func IntakeShadow(
	ctx context.Context,
	cfg *instance.Config,
	lister interface {
		ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	},
	repo providers.RepositoryRef,
	claimed []providers.WorkItem,
	log *slog.Logger,
) []IntakeRecord {
	if cfg == nil || cfg.DecisionGate == nil || len(claimed) == 0 {
		return nil
	}
	settings := cfg.DecisionGate
	if settings.EffectiveMode() != decisiongate.ModeShadow || !settings.IntakeDetection {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	gate, err := settings.Resolve(nil, nil)
	if err != nil {
		log.Warn("decisionGate intake detection disabled", "error", err.Error())
		return nil
	}
	listed, err := lister.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repo, State: "open", Limit: decisiongate.MaxIntakePool,
	})
	if err != nil {
		log.Warn("decisionGate intake candidate retrieval failed", "error", err.Error())
		return nil
	}
	open := make([]decisiongate.IntakeItem, 0, len(listed))
	for _, w := range listed {
		open = append(open, decisiongate.IntakeItem{ID: w.ID, Title: w.Title, Body: w.Body})
	}
	if len(claimed) > maxIntakePerClaim {
		claimed = claimed[:maxIntakePerClaim]
	}
	var out []IntakeRecord
	for _, w := range claimed {
		v, err := gate.EvaluateIntake(ctx, decisiongate.IntakeItem{ID: w.ID, Title: w.Title, Body: w.Body}, open)
		if err != nil {
			log.Warn("decisiongate.intake-shadow error", "item", w.ID, "error", err.Error())
		}
		out = append(out, IntakeRecord{ItemID: w.ID, Kind: v.Kind, Related: v.Related, Basis: v.Basis, Candidates: v.Candidates})
	}
	return out
}
