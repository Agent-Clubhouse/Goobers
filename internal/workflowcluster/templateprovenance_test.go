package workflowcluster

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
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
		{ID: "g/unknown", Numeric: map[string]float64{"nodes": 1}},
	}
	model := Model{Clusters: []Cluster{{ID: "novel", Medoid: points[2]}}}

	report, strata := analyzeTemplateCatalog(
		points,
		Strata{"g/declared": "reference/base", "g/unknown": "retired/template"},
		&model,
		templates,
	)

	declared := report.Workflows["g/declared"]
	if !declared.Explicit || !declared.Derived || declared.TemplateID != "reference/base" ||
		declared.Fingerprint != "sha256:base" || declared.Distance != 1 {
		t.Fatalf("declared provenance = %+v", declared)
	}
	unknown := report.Workflows["g/unknown"]
	if unknown.Explicit || unknown.Derived || unknown.TemplateID != "reference/base" || unknown.Distance != 1 {
		t.Fatalf("unshipped annotation provenance = %+v, want nearest-template fallback", unknown)
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

func TestEveryEmittedTemplateProvenanceIsCataloged(t *testing.T) {
	templates, err := shippedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	catalog := map[string]string{}
	for _, template := range templates {
		catalog[template.ID] = template.Fingerprint
	}
	seeded, err := instance.SeededWorkflowTemplates()
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, workflow := range seeded {
		ids = append(ids, workflow.ID)
	}
	for _, name := range []string{
		instance.GuidedWorkflowImplementation, instance.GuidedWorkflowBacklogCuration,
		instance.GuidedWorkflowWorkNomination, instance.GuidedWorkflowMergeReview,
	} {
		ids = append(ids, instance.ExampleTemplateProvenance(name))
	}
	for _, want := range []string{"scaffold/workflow", "demo/demo", "quickstart/v1", "starter/default-implement"} {
		if !strings.Contains(strings.Join(ids, ","), want) {
			t.Errorf("emitted IDs %v lack %q", ids, want)
		}
	}
	for _, id := range ids {
		if catalog[id] == "" {
			t.Errorf("emitted template provenance %q is not in the catalog", id)
		}
	}
}
