package journalpg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
)

func progressIndex(event journal.Event) (string, string, string, int) {
	if progress := event.Progress; progress != nil {
		return progress.AgentID, progress.RunID, progress.Stage, progress.Attempt
	}
	return "", "", "", 0
}

// Only the matching attempt's recent updates count. LIMIT bounds the indexed
// scan even for backdated clocks or a malformed producer flooding one attempt.
// The shared validator currently does not filter progress by pod ordinal (its
// newest-pod filter applies only to agent.lifecycle); retain that exact policy.
func (b *Backend) validateProgress(ctx context.Context, tx pgx.Tx, r *runOwner, event journal.Event) error {
	// PostgreSQL timestamps have microsecond precision. The indexed remainder
	// preserves the shared policy's inclusive nanosecond window boundary.
	progress := *event.Progress
	windowStart := event.Time.Add(-journal.AgentProgressRateLimitWindow)
	var count int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM (
 SELECT 1 FROM journal.run_event WHERE instance_id=$1 AND run_id=$2 AND type='agent.progress'
 AND agent_id=$3 AND agent_run=$4 AND agent_stage=$5 AND agent_attempt=$6 AND (at,at_submicro) >= ($7,$8) LIMIT $9
 ) recent`, b.config.InstanceID, r.key.RunID, progress.AgentID, progress.RunID, progress.Stage, progress.Attempt, windowStart.Truncate(time.Microsecond), windowStart.Nanosecond()%1000, journal.AgentProgressRateLimitMax).Scan(&count)
	if err != nil {
		return err
	}
	// Reuse the shared policy and diagnostic without loading historical payloads.
	recent := make([]journal.Event, count)
	for i := range recent {
		recent[i] = journal.Event{Type: journal.EventAgentProgress, Progress: &progress, Time: event.Time}
	}
	return journal.ValidateAgentProgressRate(recent, progress, event.Time)
}
