// Package configexamples exposes the canonical example definitions embedded in
// the Goobers binary.
package configexamples

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Files contains the canonical workflows and their goober definitions.
//
//go:embed gaggles/acme-web/workflows/implementation.yaml
//go:embed gaggles/acme-web/workflows/backlog-assignment.yaml
//go:embed gaggles/acme-web/workflows/backlog-curation.yaml
//go:embed gaggles/acme-web/workflows/work-nomination.yaml
//go:embed gaggles/acme-web/goobers/implementer
//go:embed gaggles/acme-web/goobers/reviewer
//go:embed gaggles/acme-web/goobers/curator
//go:embed gaggles/acme-web/goobers/nominator
var Files embed.FS

const workflowExamplePattern = "gaggles/*/workflows/*.yaml"

// WorkflowExample identifies one canonical workflow in Files.
type WorkflowExample struct {
	Name string
	Path string
	// Description is the workflow's spec.displayName, empty when unset.
	Description string
}

// workflowExampleDoc is the minimal shape needed to describe an example
// without depending on the full workflow schema.
type workflowExampleDoc struct {
	Spec struct {
		DisplayName string `yaml:"displayName"`
	} `yaml:"spec"`
}

// WorkflowExamples returns the embedded workflows in name order.
func WorkflowExamples() ([]WorkflowExample, error) {
	paths, err := fs.Glob(Files, workflowExamplePattern)
	if err != nil {
		return nil, fmt.Errorf("list embedded workflow examples: %w", err)
	}

	examples := make([]WorkflowExample, 0, len(paths))
	seen := make(map[string]string, len(paths))
	for _, workflowPath := range paths {
		name := strings.TrimSuffix(path.Base(workflowPath), path.Ext(workflowPath))
		if previous, ok := seen[name]; ok {
			return nil, fmt.Errorf("embedded workflow example %q is ambiguous: %s and %s", name, previous, workflowPath)
		}
		seen[name] = workflowPath
		description, err := workflowExampleDescription(workflowPath)
		if err != nil {
			return nil, err
		}
		examples = append(examples, WorkflowExample{Name: name, Path: workflowPath, Description: description})
	}
	sort.Slice(examples, func(i, j int) bool {
		return examples[i].Name < examples[j].Name
	})
	return examples, nil
}

// ReadWorkflowExample returns the exact embedded YAML for name.
func ReadWorkflowExample(name string) ([]byte, error) {
	examples, err := WorkflowExamples()
	if err != nil {
		return nil, err
	}
	for _, example := range examples {
		if example.Name == name {
			data, err := Files.ReadFile(example.Path)
			if err != nil {
				return nil, fmt.Errorf("read embedded workflow example %q: %w", name, err)
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("workflow example %q: %w", name, fs.ErrNotExist)
}

func workflowExampleDescription(workflowPath string) (string, error) {
	data, err := Files.ReadFile(workflowPath)
	if err != nil {
		return "", fmt.Errorf("read embedded workflow example %q: %w", workflowPath, err)
	}
	var doc workflowExampleDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("parse embedded workflow example %q: %w", workflowPath, err)
	}
	return strings.TrimSpace(doc.Spec.DisplayName), nil
}
