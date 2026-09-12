package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestRenderSupportDeltaIncludesMigrationPaths(t *testing.T) {
	previous := supportSnapshot{
		SchemaVersion: supportSnapshotSchemaVersion,
		Release:       "v1.0.0",
		Versions: []supportmatrix.Version{
			{Version: "1.0", Level: supportmatrix.LevelSupported},
			{Version: "1.1", Level: supportmatrix.LevelDeprecated, Replacement: "1.2"},
		},
	}
	current := supportSnapshot{
		SchemaVersion: supportSnapshotSchemaVersion,
		Release:       "v1.1.0",
		Versions: []supportmatrix.Version{
			{
				Version:          "1.0",
				Level:            supportmatrix.LevelDeprecated,
				UnsupportedAfter: "v1.2.0",
				Replacement:      "1.2",
			},
			{Version: "1.1", Level: supportmatrix.LevelUnsupported, Replacement: "1.2"},
		},
	}

	notes, err := renderSupportDelta(current, &previous)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## DSL support-matrix delta",
		"### Newly deprecated",
		"### Newly unsupported",
		"`goobers fix --to 1.2`",
		"before `v1.2.0`",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("release notes missing %q:\n%s", want, notes)
		}
	}
}

func TestRenderSupportDeltaSurfacesRetraction(t *testing.T) {
	previous := supportSnapshot{
		SchemaVersion: supportSnapshotSchemaVersion,
		Release:       "v0.4.0-beta.3",
		Versions: []supportmatrix.Version{
			{Version: "1.4", Level: supportmatrix.LevelUnsupported, EffectiveIn: "v0.4.0", Replacement: "2.0"},
		},
	}
	current := supportSnapshot{
		SchemaVersion: supportSnapshotSchemaVersion,
		Release:       "v0.4.0",
		Versions: []supportmatrix.Version{
			{
				Version:     "1.4",
				Level:       supportmatrix.LevelUnsupported,
				EffectiveIn: "v0.4.0",
				Replacement: "2.0",
				Retraction: &supportmatrix.Retraction{
					UnsupportedAfter: "v0.5.0",
					Release:          "v0.4.0",
					Rationale:        "the interpreter was already removed before the promised date",
				},
			},
		},
	}

	notes, err := renderSupportDelta(current, &previous)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"### Retracted commitments",
		"DSL `1.4`",
		"unsupported-after `v0.5.0`",
		"retracted in `v0.4.0`",
		"the interpreter was already removed before the promised date",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("release notes missing %q:\n%s", want, notes)
		}
	}
}

func TestSupportMatrixDeltaRequiresMigrationTarget(t *testing.T) {
	previous := supportSnapshot{
		SchemaVersion: supportSnapshotSchemaVersion,
		Release:       "v1.0.0",
		Versions:      []supportmatrix.Version{{Version: "1.0", Level: supportmatrix.LevelSupported}},
	}
	current := supportSnapshot{
		SchemaVersion: supportSnapshotSchemaVersion,
		Release:       "v1.1.0",
		Versions:      []supportmatrix.Version{{Version: "1.0", Level: supportmatrix.LevelDeprecated}},
	}
	if _, err := supportMatrixDelta(previous, current); err == nil ||
		!strings.Contains(err.Error(), "without a replacement") {
		t.Fatalf("supportMatrixDelta error = %v", err)
	}
}

func TestRenderSupportDeltaHandlesFirstAndUnchangedRelease(t *testing.T) {
	current, err := newSupportSnapshot("v1.0.0", supportmatrix.GetDSL())
	if err != nil {
		t.Fatal(err)
	}
	first, err := renderSupportDelta(current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "first DSL support matrix") {
		t.Fatalf("first release notes:\n%s", first)
	}

	previous := current
	previous.Release = "v0.9.0"
	unchanged, err := renderSupportDelta(current, &previous)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unchanged, "No DSL versions became deprecated or unsupported") {
		t.Fatalf("unchanged release notes:\n%s", unchanged)
	}
}

// TestCheckSupportMatrixForReleaseIndependentOfCheckoutTopology is #4709's
// regression fixture: the release-level check's outcome must not vary with
// how many commits a checkout sits past whichever tag git describe finds
// nearest. #4663 reported the concrete failure mode — a checkout hundreds of
// commits past a stale stable tag (v0.3.3) spuriously failed against that
// tag's release line, even though nobody was cutting v0.3.3. Every "ahead of
// a tag" shape git describe can produce is skipped identically regardless of
// which tag it names; only an exact tag (final or prerelease, optionally
// -dirty) is actually checked.
func TestCheckSupportMatrixForReleaseIndependentOfCheckoutTopology(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		skipped bool
	}{
		{"many commits past an old stable tag (#4663)", "v0.3.3-526-g9aa3996e", true},
		{"a few commits past a prerelease tag", "v0.4.0-beta.2-176-g53dd3c50d", true},
		{"a few commits past a stable tag, dirty tree", "v0.3.3-1-gabc1234-dirty", true},
		{"exactly on a stable tag", "v0.4.0", false},
		{"exactly on a prerelease tag", "v0.4.0-rc.1", false},
		{"exactly on a stable tag, dirty tree", "v0.4.0-dirty", false},
		{"no tags reachable at all", "9aa3996e", true}, // no "v" prefix either way
	} {
		t.Run(tc.name, func(t *testing.T) {
			// This only asserts the CLASSIFICATION is shape-driven (does the
			// version look like "ahead of a tag" or "exactly on one"), not
			// git-state-driven; checkSupportMatrixForRelease's actual
			// pass/fail for a checked version still depends on the compiled
			// matrix, asserted separately by TestCheckSupportMatrixForRelease.
			var skipped bool
			if !strings.HasPrefix(tc.version, "v") {
				skipped = true
			} else {
				skipped = gitDescribeAheadOfTagSuffix.MatchString(tc.version)
			}
			if skipped != tc.skipped {
				t.Fatalf("classification of %q = skipped:%t, want skipped:%t", tc.version, skipped, tc.skipped)
			}
		})
	}

	// The concrete #4663 repro: this must never fail again, regardless of
	// which tag the checkout happens to be nearest.
	if err := checkSupportMatrixForRelease("v0.3.3-526-g9aa3996e"); err != nil {
		t.Fatalf("checkSupportMatrixForRelease(v0.3.3-526-g9aa3996e) = %v, want nil (#4663)", err)
	}
}

func TestCheckSupportMatrixForRelease(t *testing.T) {
	// A final tag whose release every declared level has already reached.
	if err := checkSupportMatrixForRelease("v9.9.9"); err != nil {
		t.Fatalf("checkSupportMatrixForRelease(v9.9.9) = %v, want nil", err)
	}
	// v0.1.0 predates the transitions the compiled-in matrix declares, so the
	// levels it ships cannot be true in that release.
	err := checkSupportMatrixForRelease("v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "cannot ship in v0.1.0") {
		t.Fatalf("checkSupportMatrixForRelease(v0.1.0) = %v, want a release-level refusal", err)
	}
	for _, version := range []string{"dev", "v0.4.0-beta.2", "v0.4.0-beta.2-11-g9bcc47a2e", "9bcc47a2e", ""} {
		if err := checkSupportMatrixForRelease(version); err != nil {
			t.Errorf("checkSupportMatrixForRelease(%q) = %v, want nil for a non-final version", version, err)
		}
	}
	for _, version := range []string{"v0.1.0-rc.1", "v0.1.0-beta.2"} {
		if err := checkSupportMatrixForRelease(version); err == nil {
			t.Errorf("checkSupportMatrixForRelease(%q) bypassed the release-level refusal", version)
		}
	}
	for _, version := range []string{"v0.4.0", "v0.4.0-rc.1"} {
		if err := checkSupportMatrixForRelease(version); err != nil {
			t.Errorf("current release line %s cannot ship: %v", version, err)
		}
	}
}
