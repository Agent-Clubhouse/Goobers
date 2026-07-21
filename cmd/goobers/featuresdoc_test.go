package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workflow"
)

// TestFeatureMatrixDocUpToDate is the regen-diff guard (#430): the committed
// docs/feature-matrix.md must match what the generator produces from the
// workflow feature registry, byte for byte, so the doc cannot drift from the
// shipped binary. When a feature change is intentional, regenerate with
// UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestFeatureMatrixDocUpToDate (or
// `make docs`). Mirrors TestCLIDocsUpToDate (#1096).
func TestFeatureMatrixDocUpToDate(t *testing.T) {
	dir := docsDir(t)
	path := filepath.Join(dir, featureMatrixFile)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := writeFeatureMatrix(dir); err != nil {
			t.Fatalf("writeFeatureMatrix: %v", err)
		}
		return
	}

	want := renderFeatureMatrix()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nfeature matrix is missing or stale — regenerate with UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestFeatureMatrixDocUpToDate (or make docs)", featureMatrixFile, err)
	}
	if string(got) != want {
		t.Fatalf("docs/%s is out of date; regenerate with UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestFeatureMatrixDocUpToDate (or make docs)", featureMatrixFile)
	}
}

// TestFeatureMatrixCoversEveryFeature asserts the generated doc lists every
// feature in the registry — the doc and the `goobers features` command draw from
// the same source, so a feature can never be surfaced by one and not the other.
func TestFeatureMatrixCoversEveryFeature(t *testing.T) {
	doc := renderFeatureMatrix()
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
