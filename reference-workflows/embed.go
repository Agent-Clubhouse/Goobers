// Package referenceworkflows exposes the shipped reference workflows embedded
// in the Goobers binary.
package referenceworkflows

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Files contains the shipped reference workflow definitions.
//
//go:embed gaggles/*/workflows/*.yaml
var Files embed.FS

// WorkflowTemplate identifies one shipped reference workflow.
type WorkflowTemplate struct {
	Name string
	Path string
}

// WorkflowTemplates returns the embedded reference workflows in stable order.
func WorkflowTemplates() ([]WorkflowTemplate, error) {
	paths, err := fs.Glob(Files, "gaggles/*/workflows/*.yaml")
	if err != nil {
		return nil, fmt.Errorf("list embedded reference workflows: %w", err)
	}
	templates := make([]WorkflowTemplate, 0, len(paths))
	for _, workflowPath := range paths {
		templates = append(templates, WorkflowTemplate{
			Name: strings.TrimSuffix(path.Base(workflowPath), path.Ext(workflowPath)),
			Path: workflowPath,
		})
	}
	sort.Slice(templates, func(i, j int) bool {
		return templates[i].Path < templates[j].Path
	})
	return templates, nil
}
