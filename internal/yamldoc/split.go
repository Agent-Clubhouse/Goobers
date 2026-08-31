// Package yamldoc splits multi-document YAML streams and extracts the
// minimal kind/name metadata used to route each document, shared by the
// config validators and loaders that walk a config-as-code tree.
package yamldoc

import (
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

var documentSeparator = regexp.MustCompile(`(?m)^---\s*$`)

// Document is one YAML document in a multi-document stream.
type Document struct {
	Content    string
	LineOffset int
}

// Split splits a YAML stream into individual documents while preserving each
// document's starting line offset within the original file.
func Split(raw string) []Document {
	separators := documentSeparator.FindAllStringIndex(raw, -1)
	documents := make([]Document, 0, len(separators)+1)
	start, lineOffset := 0, 0
	for _, separator := range separators {
		documents = append(documents, Document{
			Content:    raw[start:separator[0]],
			LineOffset: lineOffset,
		})
		lineOffset += strings.Count(raw[start:separator[1]], "\n")
		start = separator[1]
	}
	return append(documents, Document{
		Content:    raw[start:],
		LineOffset: lineOffset,
	})
}

// Metadata is the minimal kind/name information required to route a document.
type Metadata struct {
	Kind       string `json:"kind"`
	DSLVersion string `json:"dslVersion"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// ExtractMetadata parses the document metadata to determine kind and name.
func ExtractMetadata(raw string) (Metadata, bool, error) {
	var meta Metadata
	if err := yaml.Unmarshal([]byte(raw), &meta); err != nil {
		return Metadata{}, false, err
	}
	return meta, meta.Kind != "", nil
}
