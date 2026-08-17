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
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/internal/supportmatrix"
)

// ErrAlreadyAtTarget is returned by Migrate when the workflow's current
// dslVersion already equals the requested target.
var ErrAlreadyAtTarget = errors.New("dslmigrate: workflow is already at the target dslVersion")

// Edge is one registered from→to migration step. Apply mutates root (the
// document's top-level mapping node) in place, returning whether it made any
// semantic changes beyond the version pin and human-readable notes describing
// each change it made — the scaffold's equivalent of a Terraform StateUpgrader
// function.
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
	// the migration. Changed includes the mandatory dslVersion pin.
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
	versionNode, _ := mapValue(root, "dslVersion")
	if versionNode != nil && versionNode.Value != "" {
		from = versionNode.Value
	}
	if from == to {
		return nil, ErrAlreadyAtTarget
	}
	edge, ok := FindEdge(from, to)
	if !ok {
		return nil, fmt.Errorf("dslmigrate: no direct migration registered from dslVersion %q to %q (chain `fix` invocations for a multi-step upgrade)", from, to)
	}

	before := string(source)
	transformed, notes := edge.Apply(root)
	var after string
	if transformed {
		setScalar(root, "dslVersion", to, "!!str")
		rendered, err := marshalDocument(&doc)
		if err != nil {
			return nil, fmt.Errorf("dslmigrate: render migrated document: %w", err)
		}
		after = rendered
	} else {
		pinned, err := pinVersion(source, versionNode, root, to)
		if err != nil {
			return nil, fmt.Errorf("dslmigrate: pin dslVersion: %w", err)
		}
		after = string(pinned)
	}
	return &Result{Before: before, After: after, Changed: before != after, Notes: notes}, nil
}

func pinVersion(source []byte, versionNode, root *yaml.Node, to string) ([]byte, error) {
	if versionNode == nil {
		return insertVersionPin(source, root, to)
	}
	if versionNode.Kind != yaml.ScalarNode || versionNode.Line < 1 || versionNode.Column < 1 {
		return nil, errors.New("dslVersion must be a scalar")
	}
	start, err := sourceOffset(source, versionNode.Line, versionNode.Column)
	if err != nil {
		return nil, err
	}
	end, err := scalarEnd(source, start, versionNode.Style)
	if err != nil {
		return nil, err
	}
	replacement := to
	switch {
	case versionNode.Style&yaml.DoubleQuotedStyle != 0:
		replacement = strconv.Quote(to)
	case versionNode.Style&yaml.SingleQuotedStyle != 0:
		replacement = "'" + to + "'"
	}
	out := make([]byte, 0, len(source)-end+start+len(replacement))
	out = append(out, source[:start]...)
	out = append(out, replacement...)
	out = append(out, source[end:]...)
	return out, nil
}

func insertVersionPin(source []byte, root *yaml.Node, to string) ([]byte, error) {
	var anchor *yaml.Node
	for _, name := range []string{"kind", "apiVersion"} {
		if value, _ := mapValue(root, name); value != nil {
			anchor = value
			break
		}
	}
	if anchor == nil || anchor.Line < 1 {
		return nil, errors.New("workflow without dslVersion must declare kind or apiVersion")
	}
	lineStart, err := sourceOffset(source, anchor.Line, 1)
	if err != nil {
		return nil, err
	}
	lineEnd := bytes.IndexByte(source[lineStart:], '\n')
	eol := []byte("\n")
	if bytes.Contains(source, []byte("\r\n")) {
		eol = []byte("\r\n")
	}
	var offset int
	var insertion []byte
	if lineEnd < 0 {
		offset = len(source)
		insertion = append(append([]byte{}, eol...), []byte(`dslVersion: `+strconv.Quote(to))...)
	} else {
		offset = lineStart + lineEnd + 1
		insertion = append([]byte(`dslVersion: `+strconv.Quote(to)), eol...)
	}
	out := make([]byte, 0, len(source)+len(insertion))
	out = append(out, source[:offset]...)
	out = append(out, insertion...)
	out = append(out, source[offset:]...)
	return out, nil
}

func sourceOffset(source []byte, line, column int) (int, error) {
	offset := 0
	for current := 1; current < line; current++ {
		next := bytes.IndexByte(source[offset:], '\n')
		if next < 0 {
			return 0, fmt.Errorf("source has no line %d", line)
		}
		offset += next + 1
	}
	offset += column - 1
	if offset < 0 || offset >= len(source) {
		return 0, fmt.Errorf("source has no column %d on line %d", column, line)
	}
	return offset, nil
}

func scalarEnd(source []byte, start int, style yaml.Style) (int, error) {
	if style&yaml.DoubleQuotedStyle != 0 || style&yaml.SingleQuotedStyle != 0 {
		quote := source[start]
		if quote != '"' && quote != '\'' {
			return 0, errors.New("quoted dslVersion does not start with a quote")
		}
		for i := start + 1; i < len(source); i++ {
			if source[i] != quote {
				continue
			}
			if quote == '\'' && i+1 < len(source) && source[i+1] == quote {
				i++
				continue
			}
			if quote == '"' {
				backslashes := 0
				for j := i - 1; j >= start && source[j] == '\\'; j-- {
					backslashes++
				}
				if backslashes%2 != 0 {
					continue
				}
			}
			return i + 1, nil
		}
		return 0, errors.New("unterminated quoted dslVersion")
	}
	end := start
	for end < len(source) && source[end] != ' ' && source[end] != '\t' && source[end] != '\r' && source[end] != '\n' && source[end] != '#' {
		end++
	}
	if end == start {
		return 0, errors.New("empty dslVersion scalar")
	}
	return end, nil
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
