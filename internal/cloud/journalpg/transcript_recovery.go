package journalpg

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
)

func (b *Backend) removeTranscriptPartials(ctx context.Context, r *runOwner, paths []string) error {
	// Cleanup can span arbitrarily many historical captures. Each batch gets
	// its own bounded fence rather than extending the ownership-transfer TX.
	for len(paths) > 0 {
		n := min(100, len(paths))
		if err := b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, _ int64) error {
			return b.removePartialPaths(ctx, tx, r, paths[:n])
		}); err != nil {
			return err
		}
		paths = paths[n:]
	}
	return nil
}
func (b *Backend) removePartialPaths(ctx context.Context, tx pgx.Tx, r *runOwner, paths []string) error {
	_, err := tx.Exec(ctx, `DELETE FROM journal.blob WHERE instance_id=$1 AND run_id=$2 AND path=ANY($5)
 AND EXISTS(SELECT 1 FROM journal.run_seq s WHERE s.instance_id=$1 AND s.run_id=$2 AND s.owner_token=$3 AND s.owner_epoch=$4)`, b.config.InstanceID, r.key.RunID, r.token, r.epoch, paths)
	return err
}
func completedTranscriptPartials(events []journal.Event) []string {
	complete := map[string]bool{}
	for _, event := range events {
		if id, ok := event.Runner["transcriptCaptureComplete"].(string); ok && id != "" {
			complete[id] = true
		}
	}
	var paths []string
	for _, event := range events {
		id, _ := event.Runner["transcriptCapture"].(string)
		if complete[id] && event.Type == journal.EventSpanRecorded && event.Ref != nil && len(id) > 0 && strings.HasPrefix(event.Ref.Digest, "sha256:") && event.Ref.Path == "spans/checkpoints/"+id+"/"+strings.TrimPrefix(event.Ref.Digest, "sha256:") {
			paths = append(paths, event.Ref.Path)
		}
	}
	return paths
}
