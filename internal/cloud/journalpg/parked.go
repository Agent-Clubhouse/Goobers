package journalpg

import (
	"context"
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// ParkedOnChild reports, for each requested run, whether it is parked on a
// child workflow: exactly journal.ParkedOnChild over the run's committed
// events, as the local scheduler's reconcile derives ReconciledRun.Suspended.
// A successor control epoch uses it so a parent parked on its child does not
// count against concurrency (internal/cloud/schedstart.OpenRun.Suspended).
//
// The result has one entry per requested run. A run with no journal yet is
// not parked: a run parks only by committing a child-wait event, and false
// keeps its slot held, the conservative direction. Any read or decode
// failure returns an error, never a partial answer.
func (b *Backend) ParkedOnChild(ctx context.Context, runIDs []string) (map[string]bool, error) {
	parked := make(map[string]bool, len(runIDs))
	for _, id := range runIDs {
		if !apiv1.ValidRunID(id) {
			return nil, fmt.Errorf("journalpg: parked-on-child lookup: %w: %q", journal.ErrNotRunDirectory, id)
		}
		parked[id] = false
	}
	if len(parked) == 0 {
		return parked, nil
	}
	ids := make([]string, 0, len(parked))
	for id := range parked {
		ids = append(ids, id)
	}
	// Only the event types the projection reads: L1 rebuilds this inventory
	// every tick, and stage outputs, progress and transcripts dominate a
	// journal's bytes.
	projected := journal.ChildWaitProjectionTypes()
	types := make([]string, len(projected))
	for i, t := range projected {
		types[i] = string(t)
	}
	rows, err := b.config.Pool.Query(ctx, `
		SELECT run_id, body FROM journal.run_event
		WHERE instance_id=$1 AND run_id=ANY($2) AND type=ANY($3)
		ORDER BY run_id, seq`, b.config.InstanceID, ids, types)
	if err != nil {
		return nil, fmt.Errorf("journalpg: parked-on-child lookup: %w", err)
	}
	defer rows.Close()
	var current string
	var events []journal.Event
	settle := func() {
		if current != "" {
			parked[current] = journal.ParkedOnChild(events)
		}
	}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("journalpg: parked-on-child lookup: %w", err)
		}
		if id != current {
			settle()
			current, events = id, events[:0]
		}
		var event journal.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, fmt.Errorf("journalpg: parked-on-child lookup: run %s: %w", id, err)
		}
		if !event.KnownSchema() {
			return nil, fmt.Errorf("journalpg: parked-on-child lookup: run %s: %w", id, journal.ErrJournalMigrationRequired)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journalpg: parked-on-child lookup: %w", err)
	}
	settle()
	return parked, nil
}
