package main

import (
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
)

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
