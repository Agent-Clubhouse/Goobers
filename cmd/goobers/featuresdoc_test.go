package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

func TestFeatureMatrixDocUpToDate(t *testing.T) {
	dir := docsDir(t)
	path := filepath.Join(dir, featureMatrixFile)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := writeFeatureMatrix(dir); err != nil {
			t.Fatalf("writeFeatureMatrix: %v", err)
		}
		return
	}

	want, err := renderFeatureMatrix()
	if err != nil {
		t.Fatalf("renderFeatureMatrix: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", featureMatrixFile, err)
	}
	if string(got) != want {
		t.Fatalf("docs/%s is out of date; regenerate with make docs", featureMatrixFile)
	}
}

func TestFeatureMatrixCoversEveryFeature(t *testing.T) {
	doc, err := renderFeatureMatrix()
	if err != nil {
		t.Fatal(err)
	}
	features := workflow.AllFeatures()
	if len(features) == 0 {
		t.Fatal("registry returned no features")
	}
	for _, feature := range features {
		if !strings.Contains(doc, "`"+string(feature.ID)+"`") {
			t.Errorf("feature %q missing from the generated matrix", feature.ID)
		}
	}
}

func TestFeatureMatrixMatchesBinaryReport(t *testing.T) {
	doc, err := renderFeatureMatrix()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range supportmatrix.GetDSL().Versions() {
		code, stdout, stderr := runArgs(t, "features", "--dsl-version", version.Version)
		if code != 0 {
			t.Fatalf("features --dsl-version %s exited %d: %s", version.Version, code, stderr)
		}
		rows, err := featureMatrixRows(workflow.AllFeatures(), version.Version)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			docRow := fmt.Sprintf("| `%s` | %s | %s | %s | %s |",
				row.Feature.ID, row.DSLVersion, row.Feature.Level, row.DSLLevel, row.Feature.SinceVersion)
			if !strings.Contains(doc, docRow) {
				t.Errorf("generated doc missing %q", docRow)
			}
			for _, value := range []string{
				string(row.Feature.ID),
				row.DSLVersion,
				string(row.Feature.Level),
				string(row.DSLLevel),
				row.Feature.SinceVersion,
			} {
				if !strings.Contains(stdout, value) {
					t.Errorf("features --dsl-version %s missing %q", version.Version, value)
				}
			}
		}
	}
}

func TestFeatureVersionDelta(t *testing.T) {
	features := []workflow.Feature{
		{ID: "added", DSLVersions: []workflow.DSLFeatureSupport{{Version: "1.1", Level: workflow.SupportPreview}}},
		{ID: "removed", DSLVersions: []workflow.DSLFeatureSupport{{Version: "1.0", Level: workflow.SupportGA}}},
		{ID: "changed", DSLVersions: []workflow.DSLFeatureSupport{
			{Version: "1.0", Level: workflow.SupportPreview},
			{Version: "1.1", Level: workflow.SupportGA},
		}},
	}
	delta, err := featureVersionDelta(features, "1.0", "1.1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(delta.Added, ","), "added"; got != want {
		t.Errorf("added = %q, want %q", got, want)
	}
	if got, want := strings.Join(delta.Removed, ","), "removed"; got != want {
		t.Errorf("removed = %q, want %q", got, want)
	}
	if got, want := strings.Join(delta.LevelChanges, ","), "changed (preview -> ga)"; got != want {
		t.Errorf("level changes = %q, want %q", got, want)
	}
}
