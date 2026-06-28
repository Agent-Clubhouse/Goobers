// Command scheduler is the Goobers work-distribution service. It routes backlog
// items to gaggles and drives work-claiming (see docs/requirements/scheduler.md).
//
// This is a skeleton entrypoint: it boots, logs, and blocks until signalled.
// Routing and claim logic are added by follow-on missions.
package main

import (
	"context"
	"log/slog"

	"github.com/goobers/goobers/internal/app"
)

func main() {
	app.Main("scheduler", func(ctx context.Context, log *slog.Logger) error {
		log.Info("scheduler skeleton online; awaiting routing wiring")
		<-ctx.Done()
		return nil
	})
}
