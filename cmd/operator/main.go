// Command operator is the Goobers Kubernetes operator. It reconciles Goobers
// CRDs (from /api) into running goober replicas and Temporal workflow
// registrations (DEP-012).
//
// Tier-3 (V2) — quarantined, not on the V0 path. See docs/ARCHITECTURE.md §11.
// Revived in V2.
package main

import (
	"context"
	"log/slog"

	"github.com/goobers/goobers/internal/app"
	"github.com/goobers/goobers/internal/operator"
)

func main() {
	app.Main("operator", func(ctx context.Context, log *slog.Logger) error {
		return operator.Run(ctx, log, operator.DefaultOptions())
	})
}
