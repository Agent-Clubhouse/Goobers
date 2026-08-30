// Package yamldoc provides utilities for parsing and splitting YAML documents.
package yamldoc

import (
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

var docSep = regexp.MustCompile(`(?m)^---\s*$`)

// Metadata represents the kind and name extracted from a YAML document header.
type Metadata struct {
	Kind       string
	Name       string
	DSLVersion string
}

// ParsedDoc represents a split and parsed YAML document segment.
type ParsedDoc struct {
	// Content is the raw YAML segment
	Content []byte
	// Meta contains extracted kind and name
	Meta Metadata
}

// SplitDocuments splits a raw YAML byte sequence on document boundaries (---)
// and returns parsed documents with extracted metadata. Empty segments are
// skipped. Returns nil if no valid documents are found.
func SplitDocuments(raw []byte) []ParsedDoc {
	if len(raw) == 0 {
		return nil
	}

	var docs []ParsedDoc
	segments := docSep.Split(string(raw), -1)

	for _, seg := range segments {
		trimmed := strings.TrimSpace(seg)
		if trimmed == "" {
			continue
		}

		// Extract kind and name metadata
		meta := extractMetadata([]byte(seg))
		if meta.Kind == "" {
			// Skip documents with no kind; validation will report them
			continue
		}

		docs = append(docs, ParsedDoc{
			Content: []byte(seg),
			Meta:    meta,
		})
	}

	return docs
}

// docMeta is the internal struct for unmarshaling YAML metadata.
type docMeta struct {
	Kind       string `json:"kind"`
	DSLVersion string `json:"dslVersion"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// extractMetadata unmarshals YAML segment to extract kind, name, and dslVersion.
func extractMetadata(segment []byte) Metadata {
	var meta docMeta
	if err := yaml.Unmarshal(segment, &meta); err != nil || meta.Kind == "" {
		return Metadata{}
	}
	return Metadata{
		Kind:       meta.Kind,
		Name:       meta.Metadata.Name,
		DSLVersion: meta.DSLVersion,
	}
}

