package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

const appliedGenerationAnnotation = "config.generation.applied"

// The instance journal is the authority for the last committed catalog, even
// when a previously retained generation is reused without producing a new run.
func lastAppliedExecutionGeneration(layout instance.Layout) (string, error) {
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type == journal.EventConfigReloaded || event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == appliedGenerationAnnotation {
			generation, _ := event.Runner["configGeneration"].(string)
			return generation, nil
		}
	}
	return "", nil
}

// Older daemons did not journal the applied generation. Their newest retained
// run is an explicit recovery source, including terminal-but-recoverable runs;
// directory modification times and unowned cached generations are not authority.
func retainedStartupGeneration(ctx context.Context, layout instance.Layout) (string, error) {
	generation, err := lastAppliedExecutionGeneration(layout)
	if err != nil || generation != "" {
		return generation, err
	}
	roots, err := layout.RunDirsContext(ctx)
	if err != nil {
		return "", err
	}
	var newest time.Time
	var runID string
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if !entry.IsDir() {
				continue
			}
			rd, err := journal.OpenRead(filepath.Join(root, entry.Name()))
			if errors.Is(err, journal.ErrNotRunDirectory) {
				continue
			}
			if err != nil {
				return "", err
			}
			id, err := rd.Identity()
			if err != nil {
				return "", err
			}
			if id.ConfigGeneration != "" && (generation == "" || id.StartedAt.After(newest) || id.StartedAt.Equal(newest) && id.RunID > runID) {
				generation, newest, runID = id.ConfigGeneration, id.StartedAt, id.RunID
			}
		}
	}
	if generation == "" {
		return "", errors.New("no applied or journal-owned generation is available; restore configuration matching a verified retained generation before restarting")
	}
	return generation, nil
}

func recoverStartupGeneration(ctx context.Context, input schedulerDefinitionsInput, retainer *configgeneration.Retainer) (schedulerDefinitionsInput, string, error) {
	generation, err := retainedStartupGeneration(ctx, input.Layout)
	if err != nil {
		return input, "", err
	}
	directory, err := retainer.UseRetained(ctx, generation)
	if err != nil {
		return input, "", fmt.Errorf("retained generation %s failed strict verification; restore its archived bytes and permission modes through storage recovery, then retry; do not delete owner journals or bypass verification: %w", generation, err)
	}
	owner, err := input.Layout.EnsureIdentity(ctx)
	if err != nil {
		return input, "", err
	}
	if err := configgeneration.VerifyDirectory(ctx, directory, generation, owner); err != nil {
		return input, "", err
	}
	set, report, err := loadConfigDirectory(directory)
	if err != nil {
		return input, "", err
	}
	digest, err := configDirectoryDigest(directory)
	if err != nil {
		return input, "", err
	}
	input.Layout = input.Layout.WithConfigDir(directory)
	input.Definitions, input.Validation, input.PinnedGeneration = set, report, generation
	return input, digest, nil
}

func generationCapacityMessage(err error) string {
	return fmt.Sprintf("%v (limits: %d generations, %d bytes); candidate configuration was not admitted. Restore configuration matching a verified retained generation, or use normal retention after its owners no longer require recovery; do not delete protected owners", err, configgeneration.MaxGenerations, configgeneration.MaxStoredBytes)
}
