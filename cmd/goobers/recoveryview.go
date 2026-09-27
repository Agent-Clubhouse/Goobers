package main

import (
	"context"
	"fmt"
	"io"
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
	// Overflow marks a snapshot held as a pinned mirror ref with no bundle
	// (#5370). It is restorable, but only while the managed repository keeps
	// the objects, so an operator reading this list has to be able to tell
	// the two tiers apart.
	Overflow bool `json:"overflow,omitempty"`
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
	// Observation: `goobers status` and the status JSON. Bounding this read by
	// the operator cap reported an over-cap inventory as "unavailable", hiding
	// the retained state of every run on exactly the instance whose retained
	// state an operator most needs to see (#5354).
	entries, unreadable, _, err := observeRecoveryInventory(ctx, layout)
	if err != nil {
		return nil, err
	}
	// An incomplete scan is still reported as unavailable, never as absence:
	// a run whose only record sits in a reservation nothing can interpret must
	// not be shown as having no recovery state. Only the CAP stopped refusing.
	if len(unreadable) > 0 {
		return nil, fmt.Errorf("recovery inventory holds %d unreadable reservation(s)", len(unreadable))
	}
	overflow, _, err := readConfiguredRecoveryOverflow(ctx, layout)
	if err != nil {
		return nil, err
	}
	views := make(map[string]*recoveryView)
	if err := addRecoveryViews(views, entries, now, false); err != nil {
		return nil, err
	}
	if err := addRecoveryViews(views, overflow, now, true); err != nil {
		return nil, err
	}
	return views, nil
}

func addRecoveryViews(views map[string]*recoveryView, entries []recovery.InventoryEntry, now time.Time, overflow bool) error {
	for _, entry := range entries {
		record, err := readRecoveryEntryRecord(entry.RecordPath)
		if err != nil {
			return err
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
			Overflow: overflow,
		})
	}
	return nil
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
// The earliest retention deadline says when the next slot can be reclaimed,
// which is what distinguishes ordinary pressure from an inventory wedged
// behind a retain floor no eviction can shorten (#4994).
//
// The cap is resolved the same way every writer into this inventory resolves
// it (#5092), so what `goobers status` reports and what a cleanup is refused
// against cannot be two different numbers for the same directory. It is
// reported, not enforced: an inventory already holding more entries than the
// cap reads as `75/8`, the occupancy the daemon's own health alarm names,
// instead of "unavailable (... full: 75 of 8 slots used ...)" — which left the
// operator unable to see the very number the alarm was firing on (#5354).
// Unreadable reservations are counted in used because they hold slots until
// they are reconciled, matching the health sampler.
func recoveryInventoryOccupancy(ctx context.Context, layout instance.Layout) (used, limit int, earliest time.Time, err error) {
	entries, unreadable, limit, err := observeRecoveryInventory(ctx, layout)
	if err != nil {
		return 0, limit, time.Time{}, err
	}
	used = len(entries) + len(unreadable)
	for _, entry := range entries {
		// The reservation's own record carries the deadline it was published
		// with; renewals live in the sidecar. Only the effective deadline says
		// when a slot actually frees.
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			return 0, limit, time.Time{}, err
		}
		if earliest.IsZero() || record.RetainUntil.Before(earliest) {
			earliest = record.RetainUntil
		}
	}
	return used, limit, earliest, nil
}

func printRecoveryInventoryOccupancy(out io.Writer, layout instance.Layout) {
	used, limit, earliest, err := recoveryInventoryOccupancy(context.Background(), layout)
	if err != nil {
		// Say WHY. An operator reading "unavailable" cannot tell a locked
		// inventory from a full one from a corrupt record, and the occupancy
		// line is the one place that failure is visible at all.
		pf(out, "recovery inventory: unavailable (%v)\n", err)
		return
	}
	line := fmt.Sprintf("recovery inventory: %d/%d", used, limit)
	// Overflow sits next to occupancy rather than in it: these snapshots hold
	// no slot, so folding them into used/limit would report an occupancy no
	// reservation agrees with (#5370).
	if overflow, err := recoveryOverflowCount(context.Background(), layout); err != nil {
		line += " (overflow unavailable)"
	} else if overflow > 0 {
		line += fmt.Sprintf(" +%d overflow (held as mirror refs until capacity frees)", overflow)
	}
	if !earliest.IsZero() {
		line += fmt.Sprintf(" (earliest retain until %s)", earliest.UTC().Format(time.RFC3339))
	}
	pf(out, "%s\n", line)
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
		if snapshot.Overflow {
			state = "overflow"
		}
		if snapshot.Expired {
			state = "expired"
		}
		pf(out, "recovery: %s (%s)\n  repository: %s\n  base: %s\n  patch: %s\n  retain until: %s\n", snapshot.Ref, state, snapshot.RepositoryKey, snapshot.BaseSHA, snapshot.PatchDigest, snapshot.RetainUntil.UTC().Format(time.RFC3339Nano))
	}
}
