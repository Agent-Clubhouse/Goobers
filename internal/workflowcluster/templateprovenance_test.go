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

func TestAnalyzeTemplateCatalogDecidesDerivationByDistance(t *testing.T) {
	templates := []shippedTemplate{
		{
			TemplateFingerprint: TemplateFingerprint{ID: "reference/alt", Fingerprint: "sha256:alt"},
			Point:               Point{ID: "reference/alt", Numeric: map[string]float64{"nodes": 2}},
		},
		{
			TemplateFingerprint: TemplateFingerprint{ID: "reference/base", Fingerprint: "sha256:base"},
			Point:               Point{ID: "reference/base", Numeric: map[string]float64{"nodes": 0}},
		},
	}
	points := []Point{
		{ID: "g/declared-near", Numeric: map[string]float64{"nodes": 1}},
		{ID: "g/declared-far", Numeric: map[string]float64{"nodes": 10}},
		{ID: "g/near", Numeric: map[string]float64{"nodes": 0}},
		{ID: "g/unknown", Numeric: map[string]float64{"nodes": 10}},
	}
	model := Model{Clusters: []Cluster{
		{ID: "far", Medoid: points[1]},
		{ID: "near", Medoid: points[0]},
	}}

	report, strata := analyzeTemplateCatalog(
		points,
		Strata{"g/declared-near": "reference/alt", "g/declared-far": "reference/base", "g/unknown": "retired/template"},
		&model,
		templates,
	)

	declaredNear := report.Workflows["g/declared-near"]
	if !declaredNear.Explicit || !declaredNear.Derived || declaredNear.TemplateID != "reference/alt" ||
		declaredNear.DeclaredTemplateID != "reference/alt" || declaredNear.Fingerprint != "sha256:alt" || declaredNear.Distance != 0.1 {
		t.Fatalf("declared-near provenance = %+v, want derived from its declared template", declaredNear)
	}
	declaredFar := report.Workflows["g/declared-far"]
	if !declaredFar.Explicit || declaredFar.Derived || declaredFar.DeclaredTemplateID != "reference/base" ||
		declaredFar.TemplateID != "reference/alt" || declaredFar.Distance != 0.8 {
		t.Fatalf("declared-far provenance = %+v, want a declared but not derived nearest-template record", declaredFar)
	}
	near := report.Workflows["g/near"]
	if near.Explicit || !near.Derived || near.TemplateID != "reference/base" || near.Distance != 0 {
		t.Fatalf("nearest provenance = %+v", near)
	}
	unknown := report.Workflows["g/unknown"]
	if unknown.Explicit || unknown.DeclaredTemplateID != "" || unknown.Derived || unknown.TemplateID != "reference/alt" {
		t.Fatalf("unshipped annotation provenance = %+v, want nearest-template fallback", unknown)
	}
	wantStrata := Strata{
		"g/declared-near": "reference/alt", "g/declared-far": BeyondTemplatesStratum,
		"g/near": "reference/base", "g/unknown": BeyondTemplatesStratum,
	}
	for id, want := range wantStrata {
		if strata[id] != want {
			t.Errorf("stratum[%s] = %q, want %q", id, strata[id], want)
		}
	}
	if len(report.BeyondTemplates) != 1 || report.BeyondTemplates[0].MedoidID != "g/declared-far" ||
		report.BeyondTemplates[0].NearestTemplate != "reference/alt" || report.BeyondTemplates[0].Distance != 0.8 {
		t.Fatalf("beyond templates = %+v, want only the annotated-but-far medoid", report.BeyondTemplates)
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
