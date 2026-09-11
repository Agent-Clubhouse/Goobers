package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/recovery"
)

// Watch reloads retention metadata on each redraw, including changes that do
// not alter the run's phase. Apply the same selection as the visible run table.
func withRecoveryStatusText(layout instance.Layout, options statusOptions, load func(context.Context, []runSummary, time.Time) (string, error)) func(context.Context, []runSummary, time.Time) (string, error) {
	return func(ctx context.Context, runs []runSummary, now time.Time) (string, error) {
		base, err := load(ctx, runs, now)
		if err != nil {
			return "", err
		}
		selected, _ := selectStatusRuns(runs, options)
		var out strings.Builder
		out.WriteString(base)
		printStatusRecovery(&out, layout, selected, now)
		return out.String(), nil
	}
}

type recoveryView struct {
	Status    string             `json:"status"`
	Snapshots []recoverySnapshot `json:"snapshots,omitempty"`
}

type recoverySnapshot struct {
	RepositoryKey string    `json:"repositoryKey"`
	Ref           string    `json:"ref"`
	BaseSHA       string    `json:"baseSha"`
	PatchDigest   string    `json:"patchDigest"`
	RetainUntil   time.Time `json:"retainUntil"`
	Expired       bool      `json:"expired"`
}

// Metadata availability is distinct from bundle integrity. Restore verifies
// actual bytes; this bounded view reports published identity and effective
// deadline without exposing local archive paths. An unreadable inventory must
// not be reported as "no recovery" or make the canonical run trace disappear.
func runRecoveryView(ctx context.Context, layout instance.Layout, runID string, now time.Time) *recoveryView {
	views, err := loadRecoveryViews(ctx, layout, now)
	if err != nil {
		return &recoveryView{Status: "unavailable"}
	}
	return views[runID]
}

func loadRecoveryViews(ctx context.Context, layout instance.Layout, now time.Time) (map[string]*recoveryView, error) {
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), 128)
	if err != nil {
		return nil, err
	}
	views := make(map[string]*recoveryView)
	for _, entry := range entries {
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			return nil, err
		}
		view := views[record.RunID]
		if view == nil {
			view = &recoveryView{Status: "retained"}
			views[record.RunID] = view
		}
		view.Snapshots = append(view.Snapshots, recoverySnapshot{
			RepositoryKey: record.RepositoryKey, Ref: record.Ref,
			BaseSHA: record.BaseSHA, PatchDigest: record.PatchDigest,
			RetainUntil: record.RetainUntil, Expired: !now.Before(record.RetainUntil),
		})
	}
	return views, nil
}

func statusRecoverySummaries(layout instance.Layout, runs []runSummary, now time.Time) []statusJSONSummary {
	summaries := statusJSONSummaries(runs)
	views, err := loadRecoveryViews(context.Background(), layout, now)
	for i := range summaries {
		if err != nil {
			summaries[i].Recovery = &recoveryView{Status: "unavailable"}
		} else {
			summaries[i].Recovery = views[summaries[i].RunID]
		}
	}
	return summaries
}

func printStatusRecovery(out io.Writer, layout instance.Layout, runs []runSummary, now time.Time) {
	if len(runs) > 0 {
		printRecoveryInventoryOccupancy(out, layout)
	}
	views, err := loadRecoveryViews(context.Background(), layout, now)
	if err != nil {
		printRecoveryView(out, &recoveryView{Status: "unavailable"})
		return
	}
	for _, run := range runs {
		if view := views[run.RunID]; view != nil {
			pf(out, "run %s recovery:\n", run.RunID)
			printRecoveryView(out, view)
		}
	}
}

// recoveryInventoryOccupancy reports the recovery inventory's current entry
// count against its configured cap (#4823 AC5), so an operator can see
// pressure building before an ordinary worktree cleanup ever gets refused.
func recoveryInventoryOccupancy(ctx context.Context, layout instance.Layout) (used, limit int, err error) {
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return 0, 0, err
	}
	limit = cfg.Retention.RecoveryEffective().MaxSnapshotsEffective()
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), limit)
	if err != nil {
		return 0, limit, err
	}
	return len(entries), limit, nil
}

func printRecoveryInventoryOccupancy(out io.Writer, layout instance.Layout) {
	used, limit, err := recoveryInventoryOccupancy(context.Background(), layout)
	if err != nil {
		pf(out, "recovery inventory: unavailable\n")
		return
	}
	pf(out, "recovery inventory: %d/%d\n", used, limit)
}

func printRecoveryView(out io.Writer, view *recoveryView) {
	if view == nil {
		return
	}
	if view.Status == "unavailable" {
		pf(out, "recovery: unavailable (inventory could not be verified)\n")
		return
	}
	for _, snapshot := range view.Snapshots {
		state := "retained"
		if snapshot.Expired {
			state = "expired"
		}
		pf(out, "recovery: %s (%s)\n  repository: %s\n  base: %s\n  patch: %s\n  retain until: %s\n", snapshot.Ref, state, snapshot.RepositoryKey, snapshot.BaseSHA, snapshot.PatchDigest, snapshot.RetainUntil.UTC().Format(time.RFC3339Nano))
	}
}
