package workflowcluster

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	configexamples "github.com/goobers/goobers/config-examples"
	"github.com/goobers/goobers/internal/instance"
	referenceworkflows "github.com/goobers/goobers/reference-workflows"
)

const (
	// TemplateProvenanceAnnotation records the shipped template used to create
	// a workflow.
	TemplateProvenanceAnnotation = instance.TemplateProvenanceAnnotation
	// StratifyTemplate groups workflows by explicit or nearest template.
	StratifyTemplate = "template"
	// DefaultTemplateDistanceThreshold is the maximum structural distance at
	// which a workflow is considered template-derived.
	DefaultTemplateDistanceThreshold = 0.35
	// BeyondTemplatesStratum is the --stratify template label for workflows
	// farther than DefaultTemplateDistanceThreshold from every shipped
	// template, whatever their annotation declares.
	BeyondTemplatesStratum = "beyond-templates"
)

// TemplateFingerprint identifies one shipped workflow template compiled into
// the binary.
type TemplateFingerprint struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

// TemplateProvenance describes a configured workflow's relationship to a
// shipped template. TemplateID is the declared template when the workflow is
// still within the derivation threshold of it, and otherwise the nearest
// shipped template. Derived is decided by structural distance alone: an
// annotation is declared provenance, not proof that the shape still matches.
type TemplateProvenance struct {
	TemplateID  string  `json:"templateId"`
	Fingerprint string  `json:"fingerprint,omitempty"`
	Distance    float64 `json:"distance"`
	// Explicit reports that the provenance annotation names a shipped
	// template; DeclaredTemplateID is that template.
	Explicit           bool   `json:"explicit"`
	DeclaredTemplateID string `json:"declaredTemplateId,omitempty"`
	Derived            bool   `json:"derived"`
}

// BeyondTemplateCluster is a cluster whose medoid is structurally far from
// every shipped template.
type BeyondTemplateCluster struct {
	ClusterID       string  `json:"clusterId"`
	MedoidID        string  `json:"medoidId"`
	NearestTemplate string  `json:"nearestTemplate"`
	Distance        float64 `json:"distance"`
}

// TemplateReport is the template provenance and convergence read contract.
type TemplateReport struct {
	DistanceThreshold float64                       `json:"distanceThreshold"`
	Catalog           []TemplateFingerprint         `json:"catalog"`
	Workflows         map[string]TemplateProvenance `json:"workflows"`
	BeyondTemplates   []BeyondTemplateCluster       `json:"beyondTemplates"`
}

type shippedTemplate struct {
	TemplateFingerprint
	Point Point
}

// shippedTemplates fingerprints, once per process, every workflow template
// embedded in this build: the reference workflows, the config examples guided
// init copies, and the starter, demo, quickstart, and scaffold workflows.
var shippedTemplates = sync.OnceValues(loadShippedTemplates)

func loadShippedTemplates() ([]shippedTemplate, error) {
	var templates []shippedTemplate
	references, err := referenceworkflows.WorkflowTemplates()
	if err != nil {
		return nil, err
	}
	for _, reference := range references {
		data, err := referenceworkflows.Files.ReadFile(reference.Path)
		if err != nil {
			return nil, fmt.Errorf("read reference template %q: %w", reference.Path, err)
		}
		template, err := templateFromYAML("reference/"+reference.Name, data)
		if err != nil {
			return nil, err
		}
		templates = append(templates, template)
	}
	examples, err := configexamples.WorkflowExamples()
	if err != nil {
		return nil, err
	}
	for _, example := range examples {
		data, err := configexamples.Files.ReadFile(example.Path)
		if err != nil {
			return nil, fmt.Errorf("read example template %q: %w", example.Path, err)
		}
		template, err := templateFromYAML(instance.ExampleTemplateProvenance(example.Name), data)
		if err != nil {
			return nil, err
		}
		templates = append(templates, template)
	}
	seeded, err := instance.SeededWorkflowTemplates()
	if err != nil {
		return nil, err
	}
	for _, workflow := range seeded {
		template, err := templateFromWorkflow(workflow.ID, workflow.Source, workflow.Workflow)
		if err != nil {
			return nil, err
		}
		templates = append(templates, template)
	}
	sort.Slice(templates, func(i, j int) bool { return templates[i].ID < templates[j].ID })
	for i := 1; i < len(templates); i++ {
		if templates[i].ID == templates[i-1].ID {
			return nil, fmt.Errorf("shipped template ID %q is not unique", templates[i].ID)
		}
	}
	return templates, nil
}

func templateFromYAML(id string, data []byte) (shippedTemplate, error) {
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return shippedTemplate{}, fmt.Errorf("decode shipped template %q: %w", id, err)
	}
	return templateFromWorkflow(id, data, workflow)
}

func templateFromWorkflow(id string, data []byte, workflow apiv1.Workflow) (shippedTemplate, error) {
	features, err := ExtractDefinition(definitionOf(workflow))
	if err != nil {
		return shippedTemplate{}, fmt.Errorf("fingerprint shipped template %q: %w", id, err)
	}
	sum := sha256.Sum256(data)
	return shippedTemplate{
		TemplateFingerprint: TemplateFingerprint{ID: id, Fingerprint: "sha256:" + hex.EncodeToString(sum[:])},
		Point:               StructuralPoint(id, features),
	}, nil
}

func analyzeTemplates(points []Point, explicit Strata, model *Model) (TemplateReport, Strata, error) {
	templates, err := shippedTemplates()
	if err != nil {
		return TemplateReport{}, nil, err
	}
	report, strata := analyzeTemplateCatalog(points, explicit, model, templates)
	return report, strata, nil
}

func analyzeTemplateCatalog(points []Point, explicit Strata, model *Model, templates []shippedTemplate) (TemplateReport, Strata) {
	report := TemplateReport{
		DistanceThreshold: DefaultTemplateDistanceThreshold,
		Catalog:           make([]TemplateFingerprint, len(templates)),
		Workflows:         make(map[string]TemplateProvenance, len(points)),
		BeyondTemplates:   []BeyondTemplateCluster{},
	}
	templateByID := make(map[string]shippedTemplate, len(templates))
	comparison := append([]Point(nil), points...)
	for i, template := range templates {
		report.Catalog[i] = template.TemplateFingerprint
		templateByID[template.ID] = template
		comparison = append(comparison, template.Point)
	}
	schema := buildSchema(comparison)
	strata := make(Strata, len(points))
	for _, point := range points {
		provenance := nearestTemplate(schema, point, templates)
		// An annotation naming a template this build does not ship is
		// ignored. One naming a shipped template is kept as a label, and
		// attributes the workflow to that template only while the shape is
		// still within the threshold of it.
		if template, ok := templateByID[explicit[point.ID]]; ok {
			provenance.Explicit, provenance.DeclaredTemplateID = true, template.ID
			if distance := round6(schema.Distance(point, template.Point)); distance <= DefaultTemplateDistanceThreshold {
				provenance.TemplateID, provenance.Fingerprint, provenance.Distance = template.ID, template.Fingerprint, distance
			}
		}
		report.Workflows[point.ID] = provenance
		strata[point.ID] = BeyondTemplatesStratum
		if provenance.Derived {
			strata[point.ID] = provenance.TemplateID
		}
	}
	if model != nil {
		for _, cluster := range model.Clusters {
			provenance, ok := report.Workflows[cluster.Medoid.ID]
			if !ok || provenance.Derived {
				continue
			}
			report.BeyondTemplates = append(report.BeyondTemplates, BeyondTemplateCluster{
				ClusterID: cluster.ID, MedoidID: cluster.Medoid.ID,
				NearestTemplate: provenance.TemplateID, Distance: provenance.Distance,
			})
		}
	}
	return report, strata
}

func nearestTemplate(schema Schema, point Point, templates []shippedTemplate) TemplateProvenance {
	best := TemplateProvenance{Distance: 1}
	for _, template := range templates {
		distance := schema.Distance(point, template.Point)
		if best.TemplateID == "" || distance < best.Distance ||
			(distance == best.Distance && template.ID < best.TemplateID) {
			best = TemplateProvenance{
				TemplateID: template.ID, Fingerprint: template.Fingerprint,
				Distance: round6(distance),
			}
		}
	}
	best.Derived = best.Distance <= DefaultTemplateDistanceThreshold
	return best
}
