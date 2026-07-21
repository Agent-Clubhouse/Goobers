package instance

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/gooberassets"
)

// ErrInvalidConfig is returned by LoadConfigDir when the config directory
// fails schema validation (CFG-023: fail closed).
var ErrInvalidConfig = errors.New("config directory failed validation")

// ConfigSet is a config/ directory's definitions, parsed into typed structs
// and pruned to the manifest's desired state (only manifest-listed gaggles
// and the goobers/workflows that belong to them).
//
// This is the tier 1-2 local counterpart to internal/configsync's Loader: it
// shares configsync's generic YAML-tree-walking shape but skips everything
// CRD-specific (client.Object, namespace/label stamping), since a local
// instance has no Kubernetes API server to reconcile against. The two loaders
// may be worth consolidating behind a shared "parse docs into typed objects"
// helper once both have settled — left as follow-up rather than refactoring
// configsync out from under an in-flight mission.
type ConfigSet struct {
	Manifest  *apiv1.Manifest
	Gaggles   []apiv1.Gaggle
	Goobers   []apiv1.Goober
	Workflows []apiv1.Workflow

	workflowSources map[workflowIdentity]string
}

type workflowIdentity struct {
	gaggle string
	name   string
}

// WorkflowSource returns the config-relative source file for a loaded workflow.
func (s *ConfigSet) WorkflowSource(gaggle, name string) (string, bool) {
	if s == nil {
		return "", false
	}
	source, ok := s.workflowSources[workflowIdentity{gaggle: gaggle, name: name}]
	return source, ok
}

// LoadConfigDir validates the config directory at dir against the canonical
// schemas (api/validate) and, if valid, parses its documents into typed
// structs pruned to the manifest's desired state.
//
// Validation includes schema shape, cross-definition bindings, workflow
// semantics, capability names, and referenced instruction files. It fails
// closed (CFG-023): on an invalid directory this returns ErrInvalidConfig and a
// nil ConfigSet, so a caller with a last-known-good set (e.g. a watching
// daemon) can leave it in place.
func LoadConfigDir(dir string) (*ConfigSet, *validate.Report, error) {
	v, err := validate.New()
	if err != nil {
		return nil, nil, fmt.Errorf("init validator: %w", err)
	}
	report, err := v.ValidateDir(dir)
	if err != nil {
		return nil, report, fmt.Errorf("validate %s: %w", dir, err)
	}
	if report.HasErrors() {
		return nil, report, ErrInvalidConfig
	}

	docs, err := readDocs(dir)
	if err != nil {
		return nil, report, err
	}
	set, err := assemble(docs)
	if err != nil {
		return nil, report, err
	}
	return set, report, nil
}

var docSep = regexp.MustCompile(`(?m)^---\s*$`)

// rawDoc is one parsed YAML document with its kind/name.
type rawDoc struct {
	kind string
	name string
	file string
	yaml []byte
}

type docMeta struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// readDocs walks root and returns every YAML document with its kind/name.
func readDocs(root string) ([]rawDoc, error) {
	var docs []rawDoc
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if gooberassets.IsSourceDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, seg := range docSep.Split(string(raw), -1) {
			if strings.TrimSpace(seg) == "" {
				continue
			}
			var meta docMeta
			if err := yaml.Unmarshal([]byte(seg), &meta); err != nil || meta.Kind == "" {
				// Schema validation already reported malformed docs; skip here.
				continue
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			docs = append(docs, rawDoc{kind: meta.Kind, name: meta.Metadata.Name, file: rel, yaml: []byte(seg)})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return docs, nil
}

// assemble parses docs into typed objects and reduces them to the manifest's
// desired state (only manifest-listed gaggles and the goobers/workflows bound
// to them).
func assemble(docs []rawDoc) (*ConfigSet, error) {
	type sourcedWorkflow struct {
		definition apiv1.Workflow
		source     string
	}
	var (
		manifest *apiv1.Manifest
		gaggles  []apiv1.Gaggle
		goobers  []apiv1.Goober
		flows    []sourcedWorkflow
	)
	for _, d := range docs {
		switch d.kind {
		case "Manifest":
			var m apiv1.Manifest
			if err := yaml.Unmarshal(d.yaml, &m); err != nil {
				return nil, fmt.Errorf("parse Manifest %s: %w", d.name, err)
			}
			if manifest != nil {
				return nil, fmt.Errorf("multiple Manifest documents (found %q and %q)", manifest.Name, m.Name)
			}
			manifest = &m
		case "Gaggle":
			var g apiv1.Gaggle
			if err := yaml.Unmarshal(d.yaml, &g); err != nil {
				return nil, fmt.Errorf("parse Gaggle %s: %w", d.name, err)
			}
			gaggles = append(gaggles, g)
		case "Goober":
			var g apiv1.Goober
			if err := yaml.Unmarshal(d.yaml, &g); err != nil {
				return nil, fmt.Errorf("parse Goober %s: %w", d.name, err)
			}
			goobers = append(goobers, g)
		case "Workflow":
			var w apiv1.Workflow
			if err := yaml.Unmarshal(d.yaml, &w); err != nil {
				return nil, fmt.Errorf("parse Workflow %s: %w", d.name, err)
			}
			flows = append(flows, sourcedWorkflow{definition: w, source: d.file})
		}
	}
	if manifest == nil {
		return nil, errors.New("no Manifest document found in config directory")
	}

	included := map[string]bool{}
	for _, name := range manifest.Spec.Gaggles {
		included[name] = true
	}

	set := &ConfigSet{Manifest: manifest, workflowSources: map[workflowIdentity]string{}}
	for i := range gaggles {
		if included[gaggles[i].Name] {
			set.Gaggles = append(set.Gaggles, gaggles[i])
		}
	}
	for i := range goobers {
		if included[goobers[i].Spec.Gaggle] {
			set.Goobers = append(set.Goobers, goobers[i])
		}
	}
	for i := range flows {
		workflow := flows[i].definition
		if included[workflow.Spec.Gaggle] {
			set.Workflows = append(set.Workflows, workflow)
			set.workflowSources[workflowIdentity{gaggle: workflow.Spec.Gaggle, name: workflow.Name}] = flows[i].source
		}
	}
	return set, nil
}
