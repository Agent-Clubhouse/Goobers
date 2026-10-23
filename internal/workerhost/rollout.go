package workerhost

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/workerrollout"
)

func (h *Host) checkRolloutLoop(ctx context.Context, status workerrollout.StatusSource, temporal workerrollout.TemporalClient) {
	timer := time.NewTimer(h.routingDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		result, err := h.reconcileRollout(ctx, status, temporal, h.cfg.VersionRetention)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			h.logf("goobers-cloud worker: error: reconcile worker-version retention: %v", err)
		case result.RetiredBuild != "":
			h.logf("goobers-cloud worker: changed %d workflow(s) pinned to goobers.%s to AUTO_UPGRADE so the blocked target version can start",
				result.Updated, result.RetiredBuild)
		}
		timer.Reset(h.rolloutInterval)
	}
}
