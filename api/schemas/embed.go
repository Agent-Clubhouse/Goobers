// Package schemas embeds the canonical JSON Schemas for the Goobers
// config-as-code objects and the runtime envelopes, so the validate CLI and any
// importing component can validate without reading files from disk.
package schemas

import "embed"

// FS holds the embedded *.schema.json files.
//
//go:embed *.schema.json
var FS embed.FS

// BaseURI is the $id base every schema uses; relative $refs resolve against it.
const BaseURI = "https://goobers.dev/schemas/"

// CandidateFindings is the versioned telemetry connector artifact schema.
const CandidateFindings = "candidate-findings-v1.schema.json"

// RemediationBrief is the current versioned PR-remediation evidence artifact schema.
const RemediationBrief = "remediation-brief-v2.schema.json"

// RemediationBriefV1 is retained because remediation brief wire versions are immutable.
const RemediationBriefV1 = "remediation-brief-v1.schema.json"

// Kind maps a config object kind to its schema file name.
var Kind = map[string]string{
	"Manifest": "manifest.schema.json",
	"Gaggle":   "gaggle.schema.json",
	"Goober":   "goober.schema.json",
	"Workflow": "workflow.schema.json",
}

// Envelope maps an envelope name to its schema file name. "artifact" names the
// shared ArtifactPointer schema that invocation/result/verdict $ref and that the
// journal (#8) imports directly.
var Envelope = map[string]string{
	"invocation": "invocation.schema.json",
	"result":     "result.schema.json",
	"verdict":    "verdict.schema.json",
	"artifact":   "artifact-pointer.schema.json",
}

// Journal maps a run-journal contract name to its schema file name — the
// versioned provenance contract (ARCHITECTURE.md §4): the events.jsonl event
// envelope and run.yaml pinned identity.
var Journal = map[string]string{
	"event": "journal-event.schema.json",
	"run":   "journal-run.schema.json",
}

// Files lists every embedded schema file name.
func Files() []string {
	return []string{
		"manifest.schema.json",
		"gaggle.schema.json",
		"goober.schema.json",
		"workflow.schema.json",
		"invocation.schema.json",
		"result.schema.json",
		"verdict.schema.json",
		"artifact-pointer.schema.json",
		CandidateFindings,
		RemediationBriefV1,
		RemediationBrief,
		"journal-event.schema.json",
		"journal-run.schema.json",
	}
}
