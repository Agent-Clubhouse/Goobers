// Package schemas embeds the canonical JSON Schemas for the Goobers
// config-as-code objects and the runtime envelopes, so the validate CLI and any
// importing component can validate without reading files from disk.
package schemas

import (
	"embed"
	"encoding/json"
	"io/fs"
	"sort"
	"strings"
)

// FS holds the embedded *.schema.json files.
//
//go:embed *.schema.json field-purposes.json
var FS embed.FS

var fieldPurposes = func() map[string]map[string]string {
	raw, err := FS.ReadFile("field-purposes.json")
	if err != nil {
		panic("read embedded field purposes: " + err.Error())
	}
	var purposes map[string]map[string]string
	if err := json.Unmarshal(raw, &purposes); err != nil {
		panic("decode embedded field purposes: " + err.Error())
	}
	return purposes
}()

// BaseURI is the $id base every schema uses; relative $refs resolve against it.
const BaseURI = "https://goobers.dev/schemas/"

const (
	// StabilityGA marks an embedded contract as generally available.
	StabilityGA = "ga"
	// InitialSinceVersion identifies contracts that predate the first tagged release.
	InitialSinceVersion = "dev"
)

// CandidateFindings is the versioned telemetry connector artifact schema.
const CandidateFindings = "candidate-findings-v1.schema.json"

// MissionControlVerdict is the provider-neutral launch verdict artifact schema.
const MissionControlVerdict = "mission-control-verdict-v1alpha1.schema.json"

// RemediationBrief is the current versioned PR-remediation evidence artifact schema.
const RemediationBrief = "remediation-brief-v3.schema.json"

// RemediationBriefV2 is retained because remediation brief wire versions are immutable.
const RemediationBriefV2 = "remediation-brief-v2.schema.json"

// RemediationBriefV1 is retained because remediation brief wire versions are immutable.
const RemediationBriefV1 = "remediation-brief-v1.schema.json"

// AgentToolkitManifest inventories the portable repository-side agent toolkit.
const AgentToolkitManifest = "agent-toolkit-manifest.schema.json"

// Diagnostics is the validate/lint machine-readable findings envelope.
const Diagnostics = "diagnostics.schema.json"

// Features is the workflow-DSL feature discovery envelope.
const Features = "features.schema.json"

// OnboardingAction is the shared versioned onboarding result envelope.
const OnboardingAction = "config-source-action.schema.json"

// ConfigSourceAction is retained as the original name of OnboardingAction.
const ConfigSourceAction = OnboardingAction

// SchemaOutput is the machine-readable envelope emitted by `goobers schema`.
const SchemaOutput = "schema-output.schema.json"

// ExplainOutput is the machine-readable envelope emitted by `goobers explain`.
const ExplainOutput = "explain.schema.json"

// HostedProgress is the GitHub Check Run live-progress contract.
const HostedProgress = "hosted-progress.schema.json"

// Kind maps a config object kind to its schema file name.
var Kind = map[string]string{
	"Manifest": "manifest.schema.json",
	"Gaggle":   "gaggle.schema.json",
	"Goober":   "goober.schema.json",
	"Workflow": "workflow.schema.json",
}

// Envelope maps an envelope name to its schema file name. "artifact" names the
// shared ArtifactPointer schema that invocation/result/verdict $ref and that the
// journal (#8) imports directly. Every v1alpha1 runtime type represented here,
// plus standalone schema-backed artifacts such as RemediationBrief, must have a
// fully populated round-trip case in api/validate/envelope_completeness_test.go.
var Envelope = map[string]string{
	"invocation": "invocation.schema.json",
	"result":     "result.schema.json",
	"verdict":    "verdict.schema.json",
	"artifact":   "artifact-pointer.schema.json",
}

// Journal maps a run-journal contract name to its schema file name — the
// versioned provenance contract (ARCHITECTURE.md §4): the directory metadata,
// events.jsonl event envelope, and run.yaml pinned identity.
var Journal = map[string]string{
	"schema": "journal-schema.schema.json",
	"event":  "journal-event.schema.json",
	"run":    "journal-run.schema.json",
}

// Notification maps the generic notification request and receipt contracts.
var Notification = map[string]string{
	"request": "notification-request.schema.json",
	"receipt": "notification-receipt.schema.json",
}

// Entry identifies one embedded schema by its CLI-facing kind and file name.
type Entry struct {
	Kind         string
	File         string
	Stability    string
	SinceVersion string
}

// Entries lists every embedded schema in stable kind order.
func Entries() []Entry {
	files, err := fs.Glob(FS, "*.schema.json")
	if err != nil {
		panic("glob embedded schemas: " + err.Error())
	}
	entries := make([]Entry, 0, len(files))
	for _, file := range files {
		entries = append(entries, Entry{
			Kind:         strings.TrimSuffix(file, ".schema.json"),
			File:         file,
			Stability:    StabilityGA,
			SinceVersion: InitialSinceVersion,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Kind < entries[j].Kind
	})
	return entries
}

// Lookup returns the embedded schema entry for a case-insensitive kind.
func Lookup(kind string) (Entry, bool) {
	for _, entry := range Entries() {
		if strings.EqualFold(entry.Kind, kind) {
			return entry, true
		}
	}
	return Entry{}, false
}

// FieldPurpose returns release-pinned guidance for legacy schema fields that
// predate description coverage.
func FieldPurpose(selector string) (string, bool) {
	root, path, ok := strings.Cut(selector, ".")
	if !ok {
		return "", false
	}
	if root == "remediation-brief-v1" || root == "remediation-brief-v2" {
		root = "remediation-brief"
	}
	fields, ok := fieldPurposes[root]
	if !ok {
		return "", false
	}
	purpose, ok := fields[path]
	if !ok && strings.HasSuffix(path, "[]") {
		purpose, ok = fields[strings.TrimSuffix(path, "[]")]
	}
	return purpose, ok
}

// Kinds lists every embedded schema kind in stable order.
func Kinds() []string {
	entries := Entries()
	kinds := make([]string, len(entries))
	for i, entry := range entries {
		kinds[i] = entry.Kind
	}
	return kinds
}

// Files lists every embedded schema file name in stable kind order.
func Files() []string {
	entries := Entries()
	files := make([]string, len(entries))
	for i, entry := range entries {
		files[i] = entry.File
	}
	return files
}
