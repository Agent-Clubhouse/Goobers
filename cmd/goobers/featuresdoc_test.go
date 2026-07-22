package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	code, stdout, stderr := runArgs(t, "features")
	if code != 0 {
		t.Fatalf("features exited %d: %s", code, stderr)
	}
	docRows := parseFeatureDocRows(t, doc)
	reportRows := parseFeatureReportRows(t, stdout)
	if len(reportRows) != len(docRows) {
		t.Fatalf("features reported %d rows, generated doc contains %d", len(reportRows), len(docRows))
	}
	for i := range docRows {
		if reportRows[i] != docRows[i] {
			t.Errorf("row %d differs:\nfeatures:      %+v\ngenerated doc: %+v", i+1, reportRows[i], docRows[i])
		}
	}
}

type featureReportRow struct {
	Feature        string
	DSLVersion     string
	FeatureSupport string
	VersionSupport string
	Since          string
}

func parseFeatureDocRows(t *testing.T, doc string) []featureReportRow {
	t.Helper()
	const header = "| Feature | DSL version | Feature support | Version support | Since app version |"
	const separator = "| --- | --- | --- | --- | --- |"
	lines := strings.Split(doc, "\n")
	headerIndex := -1
	for i, line := range lines {
		if line == header {
			headerIndex = i
			break
		}
	}
	if headerIndex < 0 || headerIndex+2 >= len(lines) {
		t.Fatalf("generated doc is missing the feature matrix header")
	}
	if lines[headerIndex+1] != separator {
		t.Fatalf("unexpected generated doc table separator %q", lines[headerIndex+1])
	}

	var rows []featureReportRow
	for _, line := range lines[headerIndex+2:] {
		if line == "" {
			break
		}
		cells := strings.Split(line, "|")
		if len(cells) != 7 || strings.TrimSpace(cells[0]) != "" || strings.TrimSpace(cells[6]) != "" {
			t.Fatalf("invalid generated doc row %q", line)
		}
		for i := 1; i <= 5; i++ {
			cells[i] = strings.TrimSpace(cells[i])
		}
		featureCell := cells[1]
		if len(featureCell) < 3 || featureCell[0] != '`' || featureCell[len(featureCell)-1] != '`' {
			t.Fatalf("invalid feature cell %q in generated doc row %q", cells[1], line)
		}
		feature := featureCell[1 : len(featureCell)-1]
		rows = append(rows, featureReportRow{
			Feature:        feature,
			DSLVersion:     cells[2],
			FeatureSupport: cells[3],
			VersionSupport: cells[4],
			Since:          cells[5],
		})
	}
	return rows
}

func parseFeatureReportRows(t *testing.T, report string) []featureReportRow {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(report), "\n")
	if len(lines) < 3 {
		t.Fatalf("features output is incomplete:\n%s", report)
	}
	wantHeader := []string{"FEATURE", "DSL", "VERSION", "FEATURE", "SUPPORT", "VERSION", "SUPPORT", "SINCE"}
	if got := strings.Fields(lines[0]); strings.Join(got, "\t") != strings.Join(wantHeader, "\t") {
		t.Fatalf("unexpected features header %q", lines[0])
	}

	separator := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			separator = i
			break
		}
	}
	if separator < 0 || separator != len(lines)-2 {
		t.Fatalf("features output must contain exactly one footer after the table:\n%s", report)
	}

	rows := make([]featureReportRow, 0, separator-1)
	for _, line := range lines[1:separator] {
		fields := strings.Fields(line)
		if len(fields) != 5 {
			t.Fatalf("invalid features row %q", line)
		}
		rows = append(rows, featureReportRow{
			Feature:        fields[0],
			DSLVersion:     fields[1],
			FeatureSupport: fields[2],
			VersionSupport: fields[3],
			Since:          fields[4],
		})
	}
	if got, want := strings.TrimSpace(lines[len(lines)-1]), fmt.Sprintf("%d feature/version row(s)", len(rows)); got != want {
		t.Fatalf("features footer = %q, want %q", got, want)
	}
	return rows
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
