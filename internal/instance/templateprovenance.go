package instance

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"text/template"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TemplateProvenanceAnnotation records the shipped template that init or
// scaffolding used to create a workflow.
const TemplateProvenanceAnnotation = "goobers.dev/template-provenance"

// ScaffoldTemplateFS embeds the `goobers scaffold` templates.
//
//go:embed templates/scaffold/*.tmpl
var ScaffoldTemplateFS embed.FS

// ScaffoldWorkflowTemplate is the ScaffoldTemplateFS path of the workflow
// scaffold.
const ScaffoldWorkflowTemplate = "templates/scaffold/workflow.yaml.tmpl"

// ShippedWorkflow is a workflow definition that init or scaffolding writes
// with a template provenance annotation.
type ShippedWorkflow struct {
	// ID is the provenance annotation value written with the workflow.
	ID string
	// Source is the exact embedded bytes the workflow is generated from.
	Source []byte
	// Workflow is the decoded definition.
	Workflow apiv1.Workflow
}

// ExampleTemplateProvenance is the provenance ID guided init writes for the
// canonical config-examples workflow name.
func ExampleTemplateProvenance(name string) string {
	return "example/" + name
}

// SeededWorkflowTemplates returns the workflows the starter, demo, and
// quickstart init seeds write plus the workflow scaffold, in ID order. Guided
// init writes ExampleTemplateProvenance IDs for config-examples workflows.
func SeededWorkflowTemplates() ([]ShippedWorkflow, error) {
	paths, err := fs.Glob(starterFS, "*/gaggles/*/workflows/*.yaml")
	if err != nil {
		return nil, fmt.Errorf("list seeded workflows: %w", err)
	}
	var templates []ShippedWorkflow
	for _, workflowPath := range paths {
		data, err := starterFS.ReadFile(workflowPath)
		if err != nil {
			return nil, fmt.Errorf("read seeded workflow %q: %w", workflowPath, err)
		}
		shipped, err := shippedWorkflow(workflowPath, data, data)
		if err != nil {
			return nil, err
		}
		templates = append(templates, shipped)
	}
	source, err := ScaffoldTemplateFS.ReadFile(ScaffoldWorkflowTemplate)
	if err != nil {
		return nil, fmt.Errorf("read workflow scaffold: %w", err)
	}
	tmpl, err := template.New(path.Base(ScaffoldWorkflowTemplate)).Parse(string(source))
	if err != nil {
		return nil, fmt.Errorf("parse workflow scaffold: %w", err)
	}
	var rendered bytes.Buffer
	placeholder := struct{ Name, Gaggle, Goober string }{"workflow", "scaffold", "goober"}
	if err := tmpl.Execute(&rendered, placeholder); err != nil {
		return nil, fmt.Errorf("render workflow scaffold: %w", err)
	}
	shipped, err := shippedWorkflow(ScaffoldWorkflowTemplate, source, rendered.Bytes())
	if err != nil {
		return nil, err
	}
	templates = append(templates, shipped)
	sort.Slice(templates, func(i, j int) bool { return templates[i].ID < templates[j].ID })
	for i := 1; i < len(templates); i++ {
		if templates[i].ID == templates[i-1].ID {
			return nil, fmt.Errorf("seeded workflow template ID %q is not unique", templates[i].ID)
		}
	}
	return templates, nil
}

func shippedWorkflow(name string, source, definition []byte) (ShippedWorkflow, error) {
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(definition, &workflow); err != nil {
		return ShippedWorkflow{}, fmt.Errorf("decode shipped workflow %q: %w", name, err)
	}
	id := workflow.Annotations[TemplateProvenanceAnnotation]
	if id == "" {
		return ShippedWorkflow{}, fmt.Errorf("shipped workflow %q lacks the %s annotation", name, TemplateProvenanceAnnotation)
	}
	return ShippedWorkflow{ID: id, Source: source, Workflow: workflow}, nil
}
