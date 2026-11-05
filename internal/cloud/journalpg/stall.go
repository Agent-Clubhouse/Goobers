package journalpg

import (
	"context"
	"errors"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/journal"
)

// StallCandidates pages nonterminal run activity from the database clock.
func (b *Backend) StallCandidates(
	ctx context.Context,
	after string,
	limit int,
) ([]journal.StallActivity, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("journalpg: stall batch limit must be 1..100")
	}
	rows, err := b.config.Pool.Query(ctx, `
		SELECT r.run_yaml,r.created_at,
		       COALESCE(s.last_append_at,r.created_at),clock_timestamp()
		FROM journal.run AS r
		JOIN journal.run_seq AS s USING (instance_id,run_id)
		JOIN journal.run_checkpoint AS c USING (instance_id,run_id)
		WHERE r.instance_id=$1 AND r.run_id>$2 AND NOT c.terminal
		ORDER BY r.run_id LIMIT $3`,
		b.config.InstanceID, after, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]journal.StallActivity, 0, limit)
	for rows.Next() {
		var raw []byte
		var startedAt time.Time
		var activity journal.StallActivity
		if err := rows.Scan(
			&raw, &startedAt, &activity.LastAppendAt, &activity.DatabaseNow,
		); err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(raw, &activity.Identity); err != nil {
			return nil, err
		}
		activity.Identity.StartedAt = startedAt.UTC()
		activity.LastAppendAt = activity.LastAppendAt.UTC()
		activity.DatabaseNow = activity.DatabaseNow.UTC()
		result = append(result, activity)
	}
	return result, rows.Err()
}

var _ journal.StallBackend = (*Backend)(nil)
