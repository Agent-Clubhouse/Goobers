package mcpio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/handoffcheck"
)

// PublicationSchema binds one declared artifact slot to the JSON Schema
// (draft 2020-12) its application/json payload must satisfy. The harness
// copies it from the workflow's trusted artifactSlots[].schemaPath; it is never
// agent-supplied.
type PublicationSchema struct {
	Slot     string          `json:"slot"`
	SchemaID string          `json:"schemaId"`
	Document json.RawMessage `json:"document"`
}

// Stable publish_output rejection categories (#6868).
const (
	// RejectInvalidManifest: the staging manifest itself is not usable.
	RejectInvalidManifest = "output_manifest_invalid"
	// RejectMissingOutput: a schema-bound slot has no manifest entry.
	RejectMissingOutput = "output_missing"
	// RejectUnreadableOutput: the payload file could not be read.
	RejectUnreadableOutput = "output_unreadable"
	// RejectSchemaViolation: the payload is not JSON or violates its schema.
	RejectSchemaViolation = "output_schema_violation"
)

// PublicationRepairInstructions is returned with every rejection so the
// producing agent repairs and republishes instead of ending its session.
const PublicationRepairInstructions = "Output was not accepted and nothing was published. Correct the listed payload file(s) and call publish_output again in this session. Do not repeat completed external actions, and do not claim completion until publication is accepted. Repair remains subject to this session's remaining budget."

// PublicationRejection is the structured, non-success publish_output result
// for a schema-bound output. It carries issue codes and JSON Pointer
// locations, never payload values.
type PublicationRejection struct {
	Status       string           `json:"status"`
	Outputs      []RejectedOutput `json:"outputs"`
	Instructions string           `json:"instructions"`
}

// RejectedOutput names one refused slot, its schema, and why.
type RejectedOutput struct {
	Slot     string               `json:"slot"`
	SchemaID string               `json:"schemaId,omitempty"`
	Category string               `json:"category"`
	Issues   []handoffcheck.Issue `json:"issues,omitempty"`
}

func (r *PublicationRejection) Error() string {
	data, err := json.Marshal(r)
	if err != nil {
		return "publish_output rejected: " + PublicationRepairInstructions
	}
	return string(data)
}

func compilePublicationSchemas(declared []PublicationSchema) (map[string]*handoffcheck.Schema, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	compiled := make(map[string]*handoffcheck.Schema, len(declared))
	for _, entry := range declared {
		if strings.TrimSpace(entry.Slot) == "" {
			return nil, fmt.Errorf("publication schema has no slot")
		}
		if _, dup := compiled[entry.Slot]; dup {
			return nil, fmt.Errorf("publication schema for slot %q is repeated", entry.Slot)
		}
		schema, err := handoffcheck.Compile(entry.SchemaID, "", entry.Document)
		if err != nil {
			return nil, fmt.Errorf("publication schema for slot %q: %w", entry.Slot, err)
		}
		compiled[entry.Slot] = schema
	}
	return compiled, nil
}

// checkPublication validates a manifest-mode publication against the stage's
// schema-bound slots before anything is written, so a refused publication
// leaves the previously accepted manifest (if any) untouched.
func (t *Toolset) checkPublication(content string) ([]PublicationReceiptOutput, error) {
	if t.schemaErr != nil {
		return nil, fmt.Errorf("publication schemas unavailable: %w", t.schemaErr)
	}
	if len(t.schemas) == 0 {
		return nil, nil
	}
	slots := make([]string, 0, len(t.schemas))
	for slot := range t.schemas {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	if verdict := handoffcheck.CheckSyntax([]byte(content)); !verdict.Valid {
		return nil, rejectAll(slots, t.schemas, RejectInvalidManifest, redactIssues(verdict.Issues))
	}
	// The same canonical validation Prepare applies at completion, so a
	// manifest this tool accepts (schemaVersion, unique names, media types,
	// paths, no unknown members) is never rejected later.
	manifest, err := artifactset.ParseManifest([]byte(content))
	if err != nil {
		return nil, rejectAll(slots, t.schemas, RejectInvalidManifest, []handoffcheck.Issue{{Code: RejectInvalidManifest, Message: err.Error()}})
	}
	entries := make(map[string]artifactset.ManifestEntry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		entries[entry.Name] = entry
	}
	rejection := &PublicationRejection{Status: "rejected", Instructions: PublicationRepairInstructions}
	var accepted []PublicationReceiptOutput
	for _, slot := range slots {
		out, digest, ok := t.checkSlot(slot, t.schemas[slot], entries)
		if !ok {
			rejection.Outputs = append(rejection.Outputs, out)
			continue
		}
		accepted = append(accepted, PublicationReceiptOutput{Slot: slot, SchemaID: out.SchemaID, PayloadDigest: digest})
	}
	if len(rejection.Outputs) > 0 {
		return nil, rejection
	}
	return accepted, nil
}

// checkSlot validates one schema-bound slot. An accepted payload also returns
// the digest of the exact bytes that were validated.
func (t *Toolset) checkSlot(slot string, schema *handoffcheck.Schema, entries map[string]artifactset.ManifestEntry) (RejectedOutput, string, bool) {
	out := RejectedOutput{Slot: slot, SchemaID: schema.ID()}
	entry, ok := entries[slot]
	if !ok {
		out.Category = RejectMissingOutput
		out.Issues = []handoffcheck.Issue{{Code: RejectMissingOutput, Message: fmt.Sprintf("manifest has no entry named %q; add one with mediaType application/json", slot)}}
		return out, "", false
	}
	if entry.MediaType != "application/json" {
		out.Category = RejectInvalidManifest
		out.Issues = []handoffcheck.Issue{{Code: "media_type_mismatch", Message: "entry mediaType must be application/json"}}
		return out, "", false
	}
	data, err := t.readStagedPayload(entry.Path)
	if err != nil {
		out.Category = RejectUnreadableOutput
		out.Issues = []handoffcheck.Issue{{Code: RejectUnreadableOutput, Message: fmt.Sprintf("payload file %q could not be read from the workspace", entry.Path)}}
		return out, "", false
	}
	verdict := schema.Check(data)
	if verdict.Valid {
		sum := sha256.Sum256(data)
		return out, "sha256:" + hex.EncodeToString(sum[:]), true
	}
	out.Category = RejectSchemaViolation
	out.Issues = redactIssues(verdict.Issues)
	return out, "", false
}

func (t *Toolset) readStagedPayload(rel string) ([]byte, error) {
	if strings.TrimSpace(rel) == "" {
		return nil, fmt.Errorf("empty path")
	}
	full, err := t.resolveInWorkspace(rel, false)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// One byte past the checker's limit lets it report too_large precisely.
	return io.ReadAll(io.LimitReader(f, handoffcheck.DefaultMaxBytes+1))
}

func rejectAll(slots []string, schemas map[string]*handoffcheck.Schema, category string, issues []handoffcheck.Issue) error {
	rejection := &PublicationRejection{Status: "rejected", Instructions: PublicationRepairInstructions}
	for _, slot := range slots {
		rejection.Outputs = append(rejection.Outputs, RejectedOutput{Slot: slot, SchemaID: schemas[slot].ID(), Category: category, Issues: issues})
	}
	return rejection
}

// redactIssues keeps codes, locations, and offsets but drops validator
// messages that may quote payload values. Messages for "required" and "type"
// name only schema properties and JSON types, so they stay actionable.
func redactIssues(issues []handoffcheck.Issue) []handoffcheck.Issue {
	out := make([]handoffcheck.Issue, len(issues))
	for i, issue := range issues {
		out[i] = issue
		if issue.Code == handoffcheck.CodeSchemaViolation && (strings.HasSuffix(issue.Keyword, "/required") || strings.HasSuffix(issue.Keyword, "/type")) {
			continue
		}
		if issue.Code != handoffcheck.CodeSchemaViolation && issue.Code != handoffcheck.CodeInvalidJSON && issue.Code != handoffcheck.CodeDuplicateKey {
			continue
		}
		out[i].Message = ""
	}
	return out
}
