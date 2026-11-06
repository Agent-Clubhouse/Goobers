package workflowcluster

import (
	"strings"
	"testing"
)

func TestShippedTemplatesHaveStableFingerprints(t *testing.T) {
	templates, err := shippedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) < 2 {
		t.Fatalf("shipped templates = %d, want reference and example workflows", len(templates))
	}
	seenReference, seenExample := false, false
	for _, template := range templates {
		seenReference = seenReference || strings.HasPrefix(template.ID, "reference/")
		seenExample = seenExample || strings.HasPrefix(template.ID, "example/")
		if !strings.HasPrefix(template.Fingerprint, "sha256:") || len(template.Fingerprint) != len("sha256:")+64 {
			t.Fatalf("template %q fingerprint = %q", template.ID, template.Fingerprint)
		}
	}
	if !seenReference || !seenExample {
		t.Fatalf("catalog lacks source groups: reference=%t example=%t", seenReference, seenExample)
	}
}

func TestAnalyzeTemplateCatalogHonorsAnnotationsAndReportsBeyondTemplates(t *testing.T) {
	templates := []shippedTemplate{{
		TemplateFingerprint: TemplateFingerprint{ID: "reference/base", Fingerprint: "sha256:base"},
		Point:               Point{ID: "reference/base", Numeric: map[string]float64{"nodes": 0}},
	}}
	points := []Point{
		{ID: "g/declared", Numeric: map[string]float64{"nodes": 1}},
		{ID: "g/near", Numeric: map[string]float64{"nodes": 0}},
		{ID: "g/novel", Numeric: map[string]float64{"nodes": 1}},
	}
	model := Model{Clusters: []Cluster{{ID: "novel", Medoid: points[2]}}}

	report, strata := analyzeTemplateCatalog(
		points,
		Strata{"g/declared": "scaffold/workflow"},
		&model,
		templates,
	)

	declared := report.Workflows["g/declared"]
	if !declared.Explicit || !declared.Derived || declared.TemplateID != "scaffold/workflow" {
		t.Fatalf("declared provenance = %+v", declared)
	}
	near := report.Workflows["g/near"]
	if near.Explicit || !near.Derived || near.TemplateID != "reference/base" || near.Distance != 0 {
		t.Fatalf("nearest provenance = %+v", near)
	}
	novel := report.Workflows["g/novel"]
	if novel.Derived || novel.Distance != 1 {
		t.Fatalf("novel provenance = %+v", novel)
	}
	if strata["g/near"] != "reference/base" || len(report.BeyondTemplates) != 1 ||
		report.BeyondTemplates[0].MedoidID != "g/novel" {
		t.Fatalf("report = %+v, strata = %+v", report, strata)
	}
}
