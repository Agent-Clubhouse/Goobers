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
