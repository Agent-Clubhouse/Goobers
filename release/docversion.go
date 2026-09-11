package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	hashiversion "github.com/hashicorp/go-version"

	"github.com/goobers/goobers/internal/supportmatrix"
)

// shippedGuideVersionPin matches a literal `VERSION=vX.Y.Z` shell assignment
// in a shipped guide — not the dynamic `VERSION="$(curl ... releases/latest
// ...)"` pattern every guide is expected to use instead (README.md,
// docs/guides/releases.md), which resolves the newest release live rather
// than baking one in at doc-authoring time.
var shippedGuideVersionPin = regexp.MustCompile(`(?m)^VERSION=(v\d+\.\d+\.\d+)\s*$`)

// checkReleasePreflight runs the release build's pre-flight refusals:
// checkSupportMatrixForRelease and checkShippedGuideVersionPins. Combined
// into one call (rather than each left as its own branch inlined in run())
// so adding the version-pin check didn't grow that already-large function's
// cyclomatic complexity — the same reason reportTelemetryPruned and
// startPeriodicSweep exist as their own functions elsewhere in this repo.
func checkReleasePreflight(version string) error {
	if err := checkSupportMatrixForRelease(version); err != nil {
		return err
	}
	repoRoot := gitOutput("rev-parse", "--show-toplevel")
	if repoRoot == "" {
		return fmt.Errorf("resolve repository root for shipped-guide version check")
	}
	return checkShippedGuideVersionPins(repoRoot)
}

// checkShippedGuideVersionPins refuses to package a release when a guide
// under docs/guides hardcodes an install-block release version literal older
// than supportmatrix.NextPlannedRelease (#4831). A stale pinned literal never
// errors and never mismatches its own checksum — it silently installs the
// wrong binary, the worst failure mode because it looks completely
// successful. This makes a guide drifting away from the dynamic
// `releases/latest` resolution pattern a release-build failure instead of a
// silent regression.
func checkShippedGuideVersionPins(repoRoot string) error {
	guidesDir := filepath.Join(repoRoot, "docs", "guides")
	entries, err := os.ReadDir(guidesDir)
	if err != nil {
		return fmt.Errorf("read shipped guides directory: %w", err)
	}
	nextPlanned, err := hashiversion.NewVersion(supportmatrix.NextPlannedRelease)
	if err != nil {
		return fmt.Errorf("parse supportmatrix.NextPlannedRelease %q: %w", supportmatrix.NextPlannedRelease, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(guidesDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		for _, match := range shippedGuideVersionPin.FindAllStringSubmatch(string(data), -1) {
			pinned, err := hashiversion.NewVersion(match[1])
			if err != nil {
				continue
			}
			if pinned.LessThan(nextPlanned) {
				return fmt.Errorf(
					"docs/guides/%s pins release version %s, older than supportmatrix.NextPlannedRelease %s; "+
						"resolve the version dynamically against releases/latest instead (see docs/guides/releases.md) "+
						"or update the pin",
					entry.Name(), match[1], supportmatrix.NextPlannedRelease)
			}
		}
	}
	return nil
}
