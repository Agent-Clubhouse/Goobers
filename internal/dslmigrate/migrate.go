// Package dslmigrate implements the mechanical, one-version-step DSL
// migrator behind `goobers fix --to <version>` (DVL-6, #866). Each registered
// Edge is a Terraform-StateUpgraders-style transform from one dslVersion to
// the next; Migrate refuses any jump that is not a single registered edge,
// and never runs a migration silently — the caller always gets a reviewable
// diff (or applies it explicitly with --write).
package dslmigrate

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/internal/supportmatrix"
)

// ErrAlreadyAtTarget is returned by Migrate when the workflow's current
// dslVersion already equals the requested target.
var ErrAlreadyAtTarget = errors.New("dslmigrate: workflow is already at the target dslVersion")

// Edge is one registered from→to migration step. Apply mutates root (the
// document's top-level mapping node) in place, returning whether it changed
// anything and human-readable notes describing each change it made — the
// scaffold's equivalent of a Terraform StateUpgrader function.
type Edge struct {
	From  string
	To    string
	Apply func(root *yaml.Node) (changed bool, notes []string)
}

// edges is the registry of one-step migrations this binary knows how to
// perform. Only the DVL-5 v_current→v_next edge exists today; a future
// version bump registers its own Edge here rather than extending this one.
var edges = []Edge{
	{From: supportmatrix.CurrentDSLVersion, To: supportmatrix.NextDSLVersion, Apply: applyCurrentToNext},
}

// FindEdge returns the registered migration from from to to, if any.
func FindEdge(from, to string) (Edge, bool) {
	for _, edge := range edges {
		if edge.From == from && edge.To == to {
			return edge, true
		}
	}
	return Edge{}, false
}

// Result is the outcome of migrating one workflow document.
type Result struct {
	// Before and After are the document's full YAML text, before and after
	// the migration. Equal (Changed=false) means the edge matched but had
	// nothing to do (e.g. every field it touches was already explicit).
	Before, After string
	Changed       bool
	// Notes describes each individual change the edge made, for the diff
	// header / commit message a caller may want to show alongside the diff.
	Notes []string
}

// Migrate parses source as a single YAML workflow document, resolves its
// current dslVersion (an absent field means supportmatrix.CurrentDSLVersion,
// mirroring internal/workflow.Compile's own default), and applies the single
// registered Edge from that version to to. It returns ErrAlreadyAtTarget when
// the workflow is already pinned to to, and a plain error naming the missing
// hop when no direct one-step Edge is registered — multi-version upgrades are
// chained by invoking fix repeatedly, never silently skipped.
func Migrate(source []byte, to string) (*Result, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(source, &doc); err != nil {
		return nil, fmt.Errorf("dslmigrate: parse workflow: %w", err)
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, errors.New("dslmigrate: workflow document has no top-level mapping")
	}

	from := supportmatrix.CurrentDSLVersion
	if versionNode, _ := mapValue(root, "dslVersion"); versionNode != nil && versionNode.Value != "" {
		from = versionNode.Value
	}
	if from == to {
		return nil, ErrAlreadyAtTarget
	}
	edge, ok := FindEdge(from, to)
	if !ok {
		return nil, fmt.Errorf("dslmigrate: no direct migration registered from dslVersion %q to %q (chain `fix` invocations for a multi-step upgrade)", from, to)
	}

	before, err := marshalDocument(&doc)
	if err != nil {
		return nil, fmt.Errorf("dslmigrate: re-render original document: %w", err)
	}
	changed, notes := edge.Apply(root)
	after, err := marshalDocument(&doc)
	if err != nil {
		return nil, fmt.Errorf("dslmigrate: render migrated document: %w", err)
	}
	return &Result{Before: before, After: after, Changed: changed, Notes: notes}, nil
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

func marshalDocument(doc *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		_ = enc.Close()
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// mapValue returns the value node for key in the mapping node n, and its
// index within n.Content (the value follows the key at index+... this is the
// value's own index, suitable for in-place mutation).
func mapValue(n *yaml.Node, key string) (*yaml.Node, int) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, -1
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1], i + 1
		}
	}
	return nil, -1
}

// setScalar sets key's value to value within the mapping node n, adding the
// key (appended, after any existing keys) if it is not already present.
func setScalar(n *yaml.Node, key, value, tag string) {
	if v, _ := mapValue(n, key); v != nil {
		v.Value = value
		if tag != "" {
			v.Tag = tag
		}
		return
	}
	n.Content = append(n.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: tag},
	)
}
