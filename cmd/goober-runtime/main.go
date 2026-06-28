// Command goober-runtime is the per-run agent runtime. It executes inside an
// ephemeral run pod: it receives the invocation envelope, drives the harness,
// and signals completion (DEP-004..DEP-007, GBO-011..GBO-013).
//
// This is a skeleton entrypoint: it boots, logs, and blocks until signalled.
// Harness orchestration and the completion handshake are added by follow-on
// missions.
package main

import (
	"context"
	"log/slog"

	"github.com/goobers/goobers/internal/app"
)

func main() {
	app.Main("goober-runtime", func(ctx context.Context, log *slog.Logger) error {
		log.Info("goober-runtime skeleton online; awaiting harness wiring")
		<-ctx.Done()
		return nil
	})
}
