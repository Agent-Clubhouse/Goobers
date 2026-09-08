package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

type ownedRunLocation struct{ dir, gaggle string }

// locateOwnedRun requires one retained run across declared gaggles and the
// legacy root. Only a legacy alias of the same directory is deduplicated;
// separate runs remain ambiguous even when their metadata is identical.
// Callers validate the run ID and the returned identity for their own plane.
func locateOwnedRun(layout instance.Layout, gaggles []string, runID string) (ownedRunLocation, error) {
	sort.Strings(gaggles)
	candidates := make([]string, 0, len(gaggles)+1)
	for _, gaggle := range gaggles {
		candidates = append(candidates, filepath.Join(layout.ForGaggle(gaggle).RunsDir(), runID))
	}
	legacy := filepath.Join(layout.RunsDir(), runID)
	candidates = append(candidates, legacy)

	found := ""
	foundGaggle := ""
	var foundInfo os.FileInfo
	for i, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "run.yaml")); err == nil {
			info, err := os.Stat(dir)
			if err != nil {
				return ownedRunLocation{}, runLookupError(http.StatusInternalServerError, "run_lookup_failed", "run could not be inspected")
			}
			if found != "" && found != dir {
				// Single-gaggle migration aliases legacy runs to the scoped
				// root. Compare directories, not run.yaml: separate journals
				// can share identical or hard-linked metadata. Only the legacy
				// compatibility path may alias an already found gaggle run.
				if dir == legacy && os.SameFile(foundInfo, info) {
					continue
				}
				return ownedRunLocation{}, runLookupError(http.StatusConflict, "ambiguous_run_id", "run ID exists in more than one gaggle")
			}
			found = dir
			foundInfo = info
			if i < len(gaggles) {
				foundGaggle = gaggles[i]
			}
		} else if !os.IsNotExist(err) {
			return ownedRunLocation{}, runLookupError(http.StatusInternalServerError, "run_lookup_failed", "run could not be inspected")
		}
	}
	if found == "" {
		return ownedRunLocation{}, runLookupError(http.StatusNotFound, "run_not_found", "run was not found")
	}
	return ownedRunLocation{dir: found, gaggle: foundGaggle}, nil
}

func runLookupError(status int, code, message string) error {
	return httpapi.NewInterventionError(status, code, message, nil)
}
