package main

import (
	"context"
	"log/slog"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/providers"
)

// intakeShadowAnnotation marks a runner.annotation as one advisory
// duplicate/dependency reading of a claimed item (#6328).
const intakeShadowAnnotation = "intake-shadow"

// recordIntakeShadow journals advisory duplicate/dependency readings for the
// claimed items when the instance opted in. It never fails or alters a claim.
func recordIntakeShadow(ctx context.Context, env backlogQueryEnv, runID string, claimed []providers.WorkItem) {
	cfg, err := instance.LoadConfig(env.layout.ConfigFile())
	if err != nil {
		return
	}
	records := runtimewiring.IntakeShadow(ctx, cfg, env.issueProvider, env.backlogRepo, claimed, slog.Default())
	if len(records) == 0 {
		return
	}
	annotations, err := openStageAnnotator(env.layout)
	if err != nil {
		return
	}
	defer func() { _ = annotations.Close() }()
	for _, r := range records {
		_ = annotations.Append(journal.Event{Type: journal.EventRunnerAnnotation, RunID: runID, Runner: map[string]any{
			"annotation":    intakeShadowAnnotation,
			"itemId":        r.ItemID,
			"repositoryKey": env.backlogRepo.CanonicalKey(),
			"verdict":       string(r.Kind),
			"related":       r.Related,
			"basis":         string(r.Basis),
			"candidates":    r.Candidates,
		}})
	}
}
