package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Older reload events predate commit-safe generation metadata. Neither their
// digest nor a run's start time proves which catalog actually became active.
func retainedStartupGeneration(ctx context.Context, layout instance.Layout) (string, error) {
	generation, err := lastAppliedExecutionGeneration(layout)
	if err != nil || generation != "" {
		return generation, err
	}
	return "", legacyGenerationRecoveryError(ctx, layout)
}

func legacyGenerationRecoveryError(ctx context.Context, layout instance.Layout) error {
	var message strings.Builder
	message.WriteString("last applied legacy catalog is ambiguous; no retained run or reload digest will be selected automatically. Select a retained catalog explicitly with: goobers up --recover-config-generation <sha256:digest> <instance>. Selection verifies archived bytes, permission modes and instance ownership before activation. Retained candidates (not yet verified):")
	root := filepath.Join(layout.Root, "config-generations")
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("%s; cannot list retained candidates: %w", message.String(), err)
	}
	count := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		decoded, err := hex.DecodeString(entry.Name())
		if err != nil || len(decoded) != 32 {
			continue
		}
		if count == configgeneration.MaxGenerations {
			message.WriteString("\nadditional entries omitted at retention limit")
			break
		}
		count++
		source := filepath.Join("config-generations", entry.Name(), "archive.json")
		modified := "unknown"
		if info, err := os.Lstat(filepath.Join(layout.Root, source)); err == nil {
			modified = info.ModTime().UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&message, "\ndigest=sha256:%s archiveModifiedAt=%s source=%s", entry.Name(), modified, filepath.ToSlash(source))
	}
	if count == 0 {
		message.WriteString(" none; restore retained custody through storage recovery")
	}
	return errors.New(message.String())
}

func recoverStartupGeneration(ctx context.Context, input schedulerDefinitionsInput, retainer *configgeneration.Retainer) (schedulerDefinitionsInput, string, error) {
	generation := input.RecoveryGeneration
	if generation == "" {
		var err error
		generation, err = retainedStartupGeneration(ctx, input.Layout)
		if err != nil {
			return input, "", err
		}
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

func journalStartupGeneration(log *journal.InstanceLog, generation, digest string) error {
	if err := log.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{
		"kind": appliedGenerationAnnotation, "configGeneration": generation, "configDigest": digest,
	}}); err != nil {
		return fmt.Errorf("journal startup generation: %w", err)
	}
	return nil
}
