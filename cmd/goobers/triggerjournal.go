package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// acceptedTriggerJournalDir includes journals staged by an interrupted prune.
// A file or symlink occupying either the parent or run path is an error, not
// evidence that no run exists. Multiple matches are likewise ambiguous.
func acceptedTriggerJournalDir(ctx context.Context, layout instance.Layout, runID string) (string, error) {
	if !apiv1.ValidRunID(runID) {
		return "", errors.New("invalid accepted run identity")
	}
	roots, err := acceptedTriggerJournalRoots(ctx, layout)
	if err != nil {
		return "", err
	}
	seen := make(map[string]bool)
	var found string
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		exists, err := realTriggerDirectory(root)
		if err != nil {
			return "", err
		}
		if !exists {
			continue
		}
		candidate := filepath.Join(root, runID)
		exists, err = realTriggerDirectory(candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("accepted run %s has multiple journal locations", runID)
		}
		found = candidate
	}
	return found, nil
}

func realTriggerDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("trigger journal path is not a real directory: %s", path)
	}
	return true, nil
}

func acceptedTriggerJournalRoots(ctx context.Context, layout instance.Layout) ([]string, error) {
	// This handles the supported legacy compatibility alias without following
	// it twice; scoped runtime roots are still checked below with Lstat.
	roots, err := layout.RunDirsContext(ctx)
	if err != nil {
		return nil, err
	}
	if layout.Gaggle() != "" {
		return append(roots, filepath.Join(filepath.Dir(layout.RunsDir()), ".telemetry-pruning")), nil
	}
	roots = append(roots, filepath.Join(layout.Root, ".telemetry-pruning"))
	exists, err := realTriggerDirectory(layout.GagglesDir())
	if err != nil || !exists {
		return roots, err
	}
	entries, err := os.ReadDir(layout.GagglesDir())
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("trigger journal gaggle root is a symlink")
		}
		if !entry.IsDir() {
			continue
		}
		parent := filepath.Join(layout.GagglesDir(), entry.Name())
		// Include roots even when runs/ is absent: only a staged journal may
		// remain, and RunDirsContext intentionally omits absent run roots.
		roots = append(roots, filepath.Join(parent, "runs"), filepath.Join(parent, ".telemetry-pruning"))
	}
	return roots, nil
}
