// Package handoffcheck is the deterministic layer that decides whether a JSON
// payload handed from one agent to another is structurally sound. Structural
// validity is answered by code, never by a model: the verdict is a pure
// function of the bytes and the schema.
//
// A valid verdict carries the payload digest and schema identity so consumers
// can be told the payload was validated. An invalid verdict carries structured,
// machine-readable issues that a producing agent can act on when retrying.
package handoffcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Issue codes. They are stable so retry prompts and telemetry can key on them.
const (
	CodeEmpty           = "empty"
	CodeTruncated       = "truncated"
	CodeInvalidJSON     = "invalid_json"
	CodeTrailingData    = "trailing_data"
	CodeDuplicateKey    = "duplicate_key"
	CodeNotJSONDocument = "not_json_document"
	CodeSchemaViolation = "schema_violation"
	CodeTooLarge        = "too_large"
)

// DefaultMaxBytes bounds the payload size the checker will inspect.
const DefaultMaxBytes = 4 << 20

// Issue is one structured problem found in a payload.
type Issue struct {
	Code string `json:"code"`
	// Path is a JSON Pointer to the offending value ("" is the document root).
	Path string `json:"path,omitempty"`
	// Keyword is the schema keyword location that failed, for schema issues.
	Keyword string `json:"keyword,omitempty"`
	Message string `json:"message"`
	// Offset is the byte offset of a syntax problem, when known.
	Offset int64 `json:"offset,omitempty"`
}

// Verdict is the deterministic outcome of checking one payload.
type Verdict struct {
	Valid         bool    `json:"valid"`
	Digest        string  `json:"digest"`
	Size          int     `json:"size"`
	SchemaID      string  `json:"schemaId,omitempty"`
	SchemaVersion string  `json:"schemaVersion,omitempty"`
	Issues        []Issue `json:"issues,omitempty"`
}

// Schema is a compiled JSON Schema plus the identity stamped on verdicts.
type Schema struct {
	id, version string
	compiled    *jsonschema.Schema
}

// Compile compiles a JSON Schema (2020-12) for use with Check.
func Compile(id, version string, schemaJSON []byte) (*Schema, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("handoffcheck: schema id is required")
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	const resource = "handoffcheck://schema.json"
	if err := compiler.AddResource(resource, bytes.NewReader(schemaJSON)); err != nil {
		return nil, fmt.Errorf("handoffcheck: schema %q: %w", id, err)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("handoffcheck: schema %q: %w", id, err)
	}
	return &Schema{id: id, version: version, compiled: compiled}, nil
}

// Check validates data as strict JSON and against the schema. It never panics
// and never depends on anything but its inputs.
func (s *Schema) Check(data []byte) Verdict { return check(s, data, DefaultMaxBytes) }

// CheckSyntax validates data as a single strict JSON document without a schema.
func CheckSyntax(data []byte) Verdict { return check(nil, data, DefaultMaxBytes) }

func check(s *Schema, data []byte, maxBytes int) Verdict {
	v := Verdict{Digest: apiv1.Digest(data), Size: len(data)}
	if s != nil {
		v.SchemaID, v.SchemaVersion = s.id, s.version
	}
	if len(data) > maxBytes {
		v.Issues = []Issue{{Code: CodeTooLarge, Message: fmt.Sprintf("payload is %d bytes, limit is %d", len(data), maxBytes)}}
		return v
	}
	if len(bytes.TrimSpace(data)) == 0 {
		v.Issues = []Issue{{Code: CodeEmpty, Message: "payload is empty"}}
		return v
	}
	if issues := checkSyntax(data); len(issues) > 0 {
		v.Issues = issues
		return v
	}
	if s != nil {
		v.Issues = s.violations(data)
	}
	v.Valid = len(v.Issues) == 0
	return v
}

func (s *Schema) violations(data []byte) []Issue {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return []Issue{{Code: CodeInvalidJSON, Message: err.Error()}}
	}
	err := s.compiled.Validate(doc)
	if err == nil {
		return nil
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return []Issue{{Code: CodeSchemaViolation, Message: err.Error()}}
	}
	var issues []Issue
	collectLeaves(verr, &issues)
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Path != issues[j].Path {
			return issues[i].Path < issues[j].Path
		}
		return issues[i].Keyword < issues[j].Keyword
	})
	return issues
}

func collectLeaves(e *jsonschema.ValidationError, out *[]Issue) {
	if len(e.Causes) == 0 {
		*out = append(*out, Issue{
			Code:    CodeSchemaViolation,
			Path:    e.InstanceLocation,
			Keyword: e.KeywordLocation,
			Message: e.Message,
		})
		return
	}
	for _, c := range e.Causes {
		collectLeaves(c, out)
	}
}

// checkSyntax walks the token stream to enforce what encoding/json.Unmarshal
// tolerates: exactly one document, no trailing bytes, and no duplicate keys.
// An abrupt end of input is reported as truncation, which is the usual cause of
// "the JSON looks corrupted" complaints.
func checkSyntax(data []byte) []Issue {
	if trimmed := bytes.TrimSpace(data); trimmed[0] != '{' && trimmed[0] != '[' && !json.Valid(trimmed) {
		hint := "payload is not a JSON document"
		if bytes.HasPrefix(trimmed, []byte("```")) {
			hint = "payload is wrapped in a markdown code fence; emit raw JSON only"
		}
		return []Issue{{Code: CodeNotJSONDocument, Message: hint}}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := walk(dec, ""); err != nil {
		return []Issue{syntaxIssue(err, dec.InputOffset())}
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return []Issue{{Code: CodeTrailingData, Offset: dec.InputOffset(), Message: "unexpected data after the JSON document"}}
		}
		return []Issue{syntaxIssue(err, dec.InputOffset())}
	}
	return nil
}

type duplicateKeyError struct{ path string }

func (e duplicateKeyError) Error() string { return "duplicate object key at " + e.path }

func syntaxIssue(err error, offset int64) Issue {
	var dup duplicateKeyError
	if errors.As(err, &dup) {
		return Issue{Code: CodeDuplicateKey, Path: dup.path, Offset: offset, Message: err.Error()}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return Issue{Code: CodeTruncated, Offset: offset, Message: "payload ends before the JSON document is complete"}
	}
	return Issue{Code: CodeInvalidJSON, Offset: offset, Message: err.Error()}
}

func walk(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := keyTok.(string)
			child := path + "/" + escapePointer(key)
			if seen[key] {
				return duplicateKeyError{path: child}
			}
			seen[key] = true
			if err := walk(dec, child); err != nil {
				return err
			}
		}
	case '[':
		for i := 0; dec.More(); i++ {
			if err := walk(dec, fmt.Sprintf("%s/%d", path, i)); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token()
	return err
}

func escapePointer(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}
