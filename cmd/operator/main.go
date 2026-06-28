// Command operator is the Goobers Kubernetes operator. It reconciles Goobers
// CRDs (from /api) into running goober replicas and Temporal workflow
// registrations (DEP-012).
//
// This is a skeleton entrypoint: it boots, logs, and blocks until signalled.
// The controller-runtime manager and reconcilers are added by follow-on
// missions that own the operator's domain logic.
package main

import (
	"context"
	"log/slog"

	"github.com/goobers/goobers/internal/app"
)

func main() {
	app.Main("operator", func(ctx context.Context, log *slog.Logger) error {
		log.Info("operator skeleton online; awaiting reconciler wiring")
		<-ctx.Done()
		return nil
	})
}
