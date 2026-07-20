package main

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

func resolveRunID(layout instance.Layout, arg string) (string, error) {
	if !apiv1.ValidRunID(arg) {
		return "", fmt.Errorf("invalid run id %q", arg)
	}
	runs, err := readservice.NewOfflineRuns(layout)
	if err != nil {
		return "", err
	}
	ids, err := runs.RunIDs(context.Background())
	if err != nil {
		return "", err
	}
	matches := make([]string, 0)
	for _, id := range ids {
		if id == arg {
			return id, nil
		}
		if strings.HasPrefix(id, arg) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("run %q: %w", arg, fs.ErrNotExist)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d runs: %s", arg, len(matches), strings.Join(matches, ", "))
	}
}

func runDirFor(layout instance.Layout, runID string) (string, error) {
	return layout.FindRunDir(runID)
}

func runsDirForRun(layout instance.Layout, runID string) (string, error) {
	dir, err := runDirFor(layout, runID)
	if err != nil {
		return "", err
	}
	return filepath.Dir(dir), nil
}
