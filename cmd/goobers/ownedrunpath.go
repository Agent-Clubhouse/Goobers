package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"sigs.k8s.io/yaml"
)

type ownedRunLocation struct{ dir, gaggle string }

// locateOwnedRun requires one retained run across declared gaggles and the
// legacy root. A unique scoped journal is authoritative over its legacy engine
// projection; separate runner journals and cross-gaggle matches remain
// ambiguous. Callers validate the run ID and returned identity for their own
// plane.
func locateOwnedRun(layout instance.Layout, gaggles []string, runID string, includeLegacy bool) (ownedRunLocation, error) {
	sort.Strings(gaggles)

	found := ""
	foundGaggle := ""
	var foundInfo os.FileInfo
	for _, gaggle := range gaggles {
		dir := filepath.Join(layout.ForGaggle(gaggle).RunsDir(), runID)
		if _, err := os.Stat(filepath.Join(dir, "run.yaml")); err == nil {
			info, err := os.Stat(dir)
			if err != nil {
				return ownedRunLocation{}, runLookupError(http.StatusInternalServerError, "run_lookup_failed", "run could not be inspected")
			}
			if found != "" && found != dir {
				return ownedRunLocation{}, runLookupError(http.StatusConflict, "ambiguous_run_id", "run ID exists in more than one gaggle")
			}
			found = dir
			foundInfo = info
			foundGaggle = gaggle
		} else if !os.IsNotExist(err) {
			return ownedRunLocation{}, runLookupError(http.StatusInternalServerError, "run_lookup_failed", "run could not be inspected")
		}
	}

	legacy := filepath.Join(layout.RunsDir(), runID)
	if !includeLegacy {
		if found == "" {
			return ownedRunLocation{}, runLookupError(http.StatusNotFound, "run_not_found", "run was not found")
		}
		return ownedRunLocation{dir: found, gaggle: foundGaggle}, nil
	}
	if _, err := os.Stat(filepath.Join(legacy, "run.yaml")); err == nil {
		legacyInfo, statErr := os.Stat(legacy)
		if statErr != nil {
			return ownedRunLocation{}, runLookupError(http.StatusInternalServerError, "run_lookup_failed", "run could not be inspected")
		}
		if found == "" {
			return ownedRunLocation{dir: legacy}, nil
		}
		// A single-gaggle migration can make the legacy and scoped paths
		// aliases of one directory. Engine mode instead produces two real
		// directories: the scoped live journal is authoritative and the flat
		// journal is only its compatibility projection. Do not weaken genuine
		// duplicate detection for runner-driven journals merely because one
		// happens to sit in the legacy root.
		if !os.SameFile(foundInfo, legacyInfo) && !sameEngineRunProjection(found, legacy, foundGaggle) {
			return ownedRunLocation{}, runLookupError(http.StatusConflict, "ambiguous_run_id", "run ID exists in more than one gaggle")
		}
	} else if !os.IsNotExist(err) {
		return ownedRunLocation{}, runLookupError(http.StatusInternalServerError, "run_lookup_failed", "run could not be inspected")
	}

	if found == "" {
		return ownedRunLocation{}, runLookupError(http.StatusNotFound, "run_not_found", "run was not found")
	}
	return ownedRunLocation{dir: found, gaggle: foundGaggle}, nil
}

// sameEngineRunProjection recognizes the temporary dual-journal shape without
// treating an arbitrary legacy copy as an alias. The scoped journal must agree
// with its directory ownership, and both identities must describe the same
// engine-owned run. Optional pins may be absent from an older compatibility
// projection, so they do not decide ownership here.
func sameEngineRunProjection(scoped, legacy, scopedGaggle string) bool {
	scopedIdentity, scopedOK := readOwnedRunIdentity(scoped)
	legacyIdentity, legacyOK := readOwnedRunIdentity(legacy)
	return scopedOK && legacyOK &&
		scopedIdentity.EngineDriven() && legacyIdentity.EngineDriven() &&
		scopedIdentity.Gaggle == scopedGaggle &&
		scopedIdentity.RunID == legacyIdentity.RunID &&
		scopedIdentity.Gaggle == legacyIdentity.Gaggle &&
		scopedIdentity.Workflow == legacyIdentity.Workflow
}

// readOwnedRunIdentity reads only immutable ownership metadata. In particular,
// it does not require or migrate schema.json: a legacy projection may predate
// that directory manifest while still carrying the current run.yaml schema.
func readOwnedRunIdentity(dir string) (journal.RunIdentity, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "run.yaml"))
	if err != nil {
		return journal.RunIdentity{}, false
	}
	var identity journal.RunIdentity
	if err := yaml.Unmarshal(raw, &identity); err != nil || !identity.KnownSchema() {
		return journal.RunIdentity{}, false
	}
	return identity, true
}

func runLookupError(status int, code, message string) error {
	return httpapi.NewInterventionError(status, code, message, nil)
}
