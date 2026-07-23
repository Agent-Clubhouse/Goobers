package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

func TestFeatureSupportDelta(t *testing.T) {
	previous := []workflow.Feature{
		{ID: "feature.deprecated", Level: workflow.SupportGA, SinceVersion: "v1.0.0"},
		{ID: "feature.ga", Level: workflow.SupportPreview, SinceVersion: "v1.0.0"},
		{ID: "feature.removed", Level: workflow.SupportDeprecated, SinceVersion: "v1.0.0"},
		{ID: "feature.unchanged", Level: workflow.SupportGA, SinceVersion: "v1.0.0"},
	}
	current := []workflow.Feature{
		{ID: "feature.unchanged", Level: workflow.SupportGA, SinceVersion: "v1.0.0"},
		{ID: "feature.removed", Level: workflow.SupportRemoved, SinceVersion: "v1.1.0"},
		{ID: "feature.new-preview", Level: workflow.SupportPreview, SinceVersion: "v1.1.0"},
		{ID: "feature.ga", Level: workflow.SupportGA, SinceVersion: "v1.1.0"},
		{ID: "feature.deprecated", Level: workflow.SupportDeprecated, SinceVersion: "v1.1.0"},
	}

	delta, err := featureSupportDelta(previous, current)
	if err != nil {
		t.Fatal(err)
	}
	if got := featureIDs(delta.NewlyGA); !slices.Equal(got, []workflow.FeatureID{"feature.ga"}) {
		t.Errorf("newly GA = %v", got)
	}
	if got := featureIDs(delta.NewlyDeprecated); !slices.Equal(got, []workflow.FeatureID{"feature.deprecated"}) {
		t.Errorf("newly deprecated = %v", got)
	}
	if got := featureIDs(delta.Removed); !slices.Equal(got, []workflow.FeatureID{"feature.removed"}) {
		t.Errorf("removed = %v", got)
	}
}

func TestFeatureSupportDeltaRejectsDroppedFeature(t *testing.T) {
	previous := []workflow.Feature{
		{ID: "feature.deleted", Level: workflow.SupportDeprecated, SinceVersion: "v1.0.0"},
	}
	if _, err := featureSupportDelta(previous, nil); err == nil || !strings.Contains(err.Error(), `retain each entry at support level "removed"`) {
		t.Fatalf("featureSupportDelta error = %v", err)
	}
}

func TestReadFeatureSnapshotRejectsInvalidRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), featureSnapshotFile)
	data := `{
  "schemaVersion": 1,
  "release": "v1.0.0",
  "features": [
    {"id":"duplicate","level":"ga","sinceVersion":"v1.0.0","history":[{"level":"ga","sinceVersion":"v1.0.0"}]},
    {"id":"duplicate","level":"ga","sinceVersion":"v1.1.0","history":[{"level":"ga","sinceVersion":"v1.1.0"}]}
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readFeatureSnapshot(path); err == nil || !strings.Contains(err.Error(), "duplicate DSL feature") {
		t.Fatalf("readFeatureSnapshot error = %v, want duplicate feature error", err)
	}
}

func TestWriteReleaseMetadata(t *testing.T) {
	previous, err := newFeatureSnapshot("v0.0.0", workflow.AllFeatures())
	if err != nil {
		t.Fatal(err)
	}
	previousJSON, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	previousPath := filepath.Join(t.TempDir(), "previous.json")
	if err := os.WriteFile(previousPath, previousJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	previousSupport, err := newSupportSnapshot("v0.0.0", supportmatrix.GetDSL())
	if err != nil {
		t.Fatal(err)
	}
	previousSupportJSON, err := json.Marshal(previousSupport)
	if err != nil {
		t.Fatal(err)
	}
	previousSupportPath := filepath.Join(t.TempDir(), "previous-support.json")
	if err := os.WriteFile(previousSupportPath, previousSupportJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	notesPath, snapshotPaths, err := writeReleaseMetadata("v0.1.0", previousPath, previousSupportPath, out)
	if err != nil {
		t.Fatalf("writeReleaseMetadata: %v", err)
	}
	notes, err := os.ReadFile(notesPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Goobers v0.1.0", "Compared with `v0.0.0`.", "DSL support-matrix delta", "Support policy for external consumers"} {
		if !strings.Contains(string(notes), want) {
			t.Errorf("release notes missing %q:\n%s", want, notes)
		}
	}
	snapshot, err := readFeatureSnapshot(snapshotPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Release != "v0.1.0" || len(snapshot.Features) != len(workflow.AllFeatures()) {
		t.Errorf("snapshot metadata = release %q, %d features", snapshot.Release, len(snapshot.Features))
	}
	supportSnapshot, err := readSupportSnapshot(snapshotPaths[1])
	if err != nil {
		t.Fatal(err)
	}
	if supportSnapshot.Release != "v0.1.0" {
		t.Errorf("support snapshot release = %q", supportSnapshot.Release)
	}
}

func TestSampleReleaseNoteUpToDate(t *testing.T) {
	previous, current := sampleFeatureSnapshots(t)
	supportNotes, _, err := supportReleaseMetadata(current.Release, "")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderReleaseNotes(current, &previous, supportNotes)
	if err != nil {
		t.Fatal(err)
	}
	want := "<!-- Illustrative generated output; this is not a published release. -->\n\n" + rendered
	path := filepath.Join("..", "docs", "releases", "sample-release-notes.md")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sample release note: %v", err)
	}
	if string(got) != want {
		t.Fatal("sample release note is stale; regenerate with UPDATE_GOLDEN=1 go test ./release -run TestSampleReleaseNoteUpToDate")
	}
}

func sampleFeatureSnapshots(t *testing.T) (featureSnapshot, featureSnapshot) {
	t.Helper()
	transition := func(level workflow.SupportLevel, version string) workflow.SupportTransition {
		return workflow.SupportTransition{Level: level, SinceVersion: version}
	}
	previous, err := newFeatureSnapshot("v0.1.0", []workflow.Feature{
		{
			ID: "stage.shell", Level: workflow.SupportGA, SinceVersion: "v0.1.0",
			History: []workflow.SupportTransition{transition(workflow.SupportGA, "v0.1.0")},
		},
		{
			ID: "trigger.schedule", Level: workflow.SupportPreview, SinceVersion: "v0.1.0",
			History: []workflow.SupportTransition{transition(workflow.SupportPreview, "v0.1.0")},
		},
		{
			ID: "trigger.webhook", Level: workflow.SupportDeprecated, SinceVersion: "v0.1.0",
			Replacement: "trigger.schedule", RemovalTargetVersion: "v0.2.0",
			History: []workflow.SupportTransition{
				transition(workflow.SupportGA, "v0.0.0"),
				transition(workflow.SupportDeprecated, "v0.1.0"),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := newFeatureSnapshot("v0.2.0", []workflow.Feature{
		{
			ID: "stage.shell", Level: workflow.SupportDeprecated, SinceVersion: "v0.2.0",
			Replacement: "stage.run", RemovalTargetVersion: "v0.3.0",
			History: []workflow.SupportTransition{
				transition(workflow.SupportGA, "v0.1.0"),
				transition(workflow.SupportDeprecated, "v0.2.0"),
			},
		},
		{
			ID: "trigger.schedule", Level: workflow.SupportGA, SinceVersion: "v0.2.0",
			History: []workflow.SupportTransition{
				transition(workflow.SupportPreview, "v0.1.0"),
				transition(workflow.SupportGA, "v0.2.0"),
			},
		},
		{
			ID: "trigger.webhook", Level: workflow.SupportRemoved, SinceVersion: "v0.2.0",
			LastSupportingVersion: "v0.1.0",
			History: []workflow.SupportTransition{
				transition(workflow.SupportGA, "v0.0.0"),
				transition(workflow.SupportDeprecated, "v0.1.0"),
				transition(workflow.SupportRemoved, "v0.2.0"),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return previous, current
}

func featureIDs(features []workflow.Feature) []workflow.FeatureID {
	ids := make([]workflow.FeatureID, 0, len(features))
	for _, feature := range features {
		ids = append(ids, feature.ID)
	}
	return ids
}
