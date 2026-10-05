package workbench

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// ObjectiveMetadata is the closed Goobers-owned frontmatter namespace. Other
// frontmatter belongs to its author and is retained during metadata proposals.
type ObjectiveMetadata struct {
	SchemaVersion string `json:"schemaVersion" yaml:"schemaVersion"`
	ObjectiveID   string `json:"objectiveId" yaml:"objectiveId"`
	Title         string `json:"title" yaml:"title"`
	Edges         []Edge `json:"edges,omitempty" yaml:"edges,omitempty"`
}

// Document represents one bounded revision-pinned Markdown read. A nil objective
// means ordinary reference material, not an inferred planning objective.
type Document struct {
	Objective *ObjectiveMetadata
	Body      []byte
	header    *yaml.Node
	bom       []byte
	newline   string
}

// ParseDocument checks explicit identity and same-gaggle relationships. It never
// executes Markdown, follows links, fetches sources or infers contribution edges.
func ParseDocument(raw []byte, scope Scope, binding string) (Document, error) {
	if err := scope.Validate(); err != nil {
		return Document{}, err
	}
	if !scope.Bindings[binding] {
		return Document{}, errors.New("workbench: document binding is not configured")
	}
	if len(raw) > MaxSourceBytes || !utf8.Valid(raw) {
		return Document{}, errors.New("workbench: Markdown is not bounded UTF-8")
	}
	doc, header, err := splitFrontmatter(raw)
	if err != nil || header == nil {
		return doc, err
	}
	if len(bytes.TrimSpace(header)) == 0 {
		return doc, nil
	}
	doc.header, err = parseSourceYAML(header)
	if err != nil {
		return Document{}, err
	}
	for i := 0; i < len(doc.header.Content); i += 2 {
		if doc.header.Content[i].Value != "goobers" {
			continue
		}
		var objective ObjectiveMetadata
		if err := decodeClosed(doc.header.Content[i+1], &objective); err != nil {
			return Document{}, err
		}
		if err := validateObjective(scope, binding, objective); err != nil {
			return Document{}, err
		}
		doc.Objective = &objective
	}
	return doc, nil
}

func validateObjective(scope Scope, binding string, value ObjectiveMetadata) error {
	if value.SchemaVersion != "objectives/v1" || !validPersistentID(value.ObjectiveID, "obj-") || !textValue(value.Title, 512) {
		return errors.New("workbench: invalid objective metadata version, ID or title")
	}
	ref := NodeRef{GaggleID: scope.GaggleID, SourceBindingID: binding, Kind: "objective-document", SourceID: value.ObjectiveID}
	if err := scope.ValidateRef(ref); err != nil {
		return err
	}
	if err := validateEdges(scope, value.Edges); err != nil {
		return err
	}
	for _, edge := range value.Edges {
		if edge.From != ref {
			return errors.New("workbench: frontmatter may own only this document's outgoing edges")
		}
	}
	return nil
}

// ProposeObjectiveMetadata returns candidate source bytes for a policy-governed
// PR. It performs no write. Existing identity cannot change; unrelated YAML
// values/comments are retained semantically and Markdown body bytes stay exact.
// YAML formatting may normalize, so callers must show the resulting diff.
func ProposeObjectiveMetadata(raw []byte, scope Scope, binding string, proposed ObjectiveMetadata) ([]byte, error) {
	doc, err := ParseDocument(raw, scope, binding)
	if err != nil {
		return nil, err
	}
	if err := validateObjective(scope, binding, proposed); err != nil {
		return nil, err
	}
	if doc.Objective != nil && doc.Objective.ObjectiveID != proposed.ObjectiveID {
		return nil, errors.New("workbench: existing objective identity is immutable")
	}
	if doc.header == nil {
		doc.header = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	var value yaml.Node
	if err := value.Encode(proposed); err != nil {
		return nil, err
	}
	replaceObjectiveNode(doc.header, &value)
	header, err := yaml.Marshal(doc.header)
	if err != nil {
		return nil, err
	}
	if doc.newline == "\r\n" {
		header = bytes.ReplaceAll(header, []byte("\n"), []byte("\r\n"))
	}
	result := append([]byte{}, doc.bom...)
	result = append(result, []byte("---"+doc.newline)...)
	result = append(result, header...)
	result = append(result, []byte("---"+doc.newline)...)
	result = append(result, doc.Body...)
	if len(result) > MaxSourceBytes {
		return nil, errors.New("workbench: proposed document exceeds source byte limit")
	}
	return result, nil
}

func replaceObjectiveNode(header, value *yaml.Node) {
	for i := 0; i < len(header.Content); i += 2 {
		if header.Content[i].Value == "goobers" {
			header.Content[i+1] = value
			return
		}
	}
	header.Content = append(header.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "goobers"}, value)
}

func splitFrontmatter(raw []byte) (Document, []byte, error) {
	doc := Document{newline: "\n"}
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		doc.bom, raw = raw[:3], raw[3:]
	}
	doc.Body = append([]byte{}, raw...)
	firstEnd := bytes.IndexByte(raw, '\n')
	if firstEnd < 0 || strings.TrimSuffix(string(raw[:firstEnd]), "\r") != "---" {
		return doc, nil, nil
	}
	if firstEnd > 0 && raw[firstEnd-1] == '\r' {
		doc.newline = "\r\n"
	}
	for start := firstEnd + 1; start < len(raw); {
		end := bytes.IndexByte(raw[start:], '\n')
		next := len(raw)
		if end < 0 {
			end = len(raw)
		} else {
			end += start
			next = end + 1
		}
		line := strings.TrimSuffix(string(raw[start:end]), "\r")
		if line == "---" || line == "..." {
			doc.Body = append([]byte{}, raw[next:]...)
			return doc, raw[firstEnd+1 : start], nil
		}
		start = next
	}
	return Document{}, nil, errors.New("workbench: unterminated frontmatter")
}

// LocatedObjective identifies a live source location in a coherent scan. Moves
// retain their ID; two simultaneously observed locations with that ID conflict.
type LocatedObjective struct {
	Binding, Path, Revision string
	Objective               ObjectiveMetadata
}

// ValidateObjectiveSet detects duplicate live IDs without choosing a winner.
// Call only for the coherent authorized source set; incomplete reads do not
// prove that a missing old location was deleted or moved.
func ValidateObjectiveSet(scope Scope, values []LocatedObjective) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if len(values) > 50000 {
		return errors.New("workbench: objective projection exceeds node bound")
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !validSourcePath(value.Path) || !textValue(value.Revision, 512) {
			return errors.New("workbench: objective location needs a pinned source revision")
		}
		if err := validateObjective(scope, value.Binding, value.Objective); err != nil {
			return err
		}
		if seen[value.Objective.ObjectiveID] {
			return fmt.Errorf("workbench: conflicting live objective ID %s", value.Objective.ObjectiveID)
		}
		seen[value.Objective.ObjectiveID] = true
	}
	return nil
}
