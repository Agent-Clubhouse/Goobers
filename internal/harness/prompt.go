package harness

import (
	"fmt"
	"strings"
)

// renderPrompt composes the harness input: the goober's persona/instructions,
// the stage goal and context, and an explicit completion-contract directive —
// the local analog of the "completion tool" (GBO-013) — telling the harness
// exactly where to write its result/verdict JSON. The concrete CLI input
// format (flags, stdin vs. file) is an adapter implementation detail
// (GBO-051); this is a plain-text rendering any harness that accepts a
// prompt string/file can consume.
func renderPrompt(req RunRequest) string {
	var b strings.Builder
	if req.Instructions != "" {
		b.WriteString(strings.TrimSpace(req.Instructions))
	}
	if req.Envelope.InstructionAddendum != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## One-off instruction addendum\n\n")
		b.WriteString(strings.TrimSpace(req.Envelope.InstructionAddendum))
		b.WriteString("\n\nThis addendum applies only to this invocation and does not modify the workflow definition.")
	}
	if b.Len() > 0 {
		b.WriteString("\n\n---\n\n")
	}
	fmt.Fprintf(&b, "## Task\n\n%s\n\n", req.Envelope.Goal)

	if len(req.Envelope.ContextPointers) > 0 {
		b.WriteString("## Context\n\n")
		for _, cp := range req.Envelope.ContextPointers {
			// An Artifact pointer resolved into the workspace (#121) is
			// actionable: point the harness at the actual file it can read.
			// Anything else (External, or an Artifact that for some reason
			// wasn't resolved) falls back to the bare name, unchanged.
			if path, ok := req.ContextPaths[cp.Name]; ok {
				fmt.Fprintf(&b, "- %s: available at `%s`\n", cp.Name, path)
				continue
			}
			fmt.Fprintf(&b, "- %s\n", cp.Name)
		}
		b.WriteString("\n")
	}

	completionKind, schemaHint := completionContract(req)
	fmt.Fprintf(&b, "## Completion contract\n\n"+
		"When you are finished, write your %s as JSON to `%s` (relative to your "+
		"working directory), matching this shape:\n\n%s\n\n"+
		"Do not consider the task complete until this file exists and is valid JSON. "+
		"Never print credentials or tokens to stdout/stderr.\n",
		completionKind, req.CompletionPath, schemaHint)

	return b.String()
}

func renderCompletionRecoveryPrompt(req RunRequest) string {
	completionKind, schemaHint := completionContract(req)
	return fmt.Sprintf(
		"Your previous turn ended without writing the mandatory completion file. "+
			"Do not repeat completed work or make unrelated changes. Inspect the current state, then write "+
			"the final %s as valid JSON to `%s` "+
			"(relative to the working directory), matching this shape:\n\n%s\n\n"+
			"Report the actual outcome; do not claim success unless the task is complete. "+
			"Do not finish this turn until the file exists and is valid JSON.",
		completionKind, req.CompletionPath, schemaHint,
	)
}

func completionContract(req RunRequest) (string, string) {
	if req.Mode == ModeReview {
		return "verdict", verdictShapeHint
	}
	return "result", resultShapeHint
}

// resultShapeHint deliberately omits "error", "artifacts", and "transcript" from the base
// shape. "error" is required only on a "failure"/"blocked" status, and an empty
// error object on a successful result fails the schema's errorInfo minLength:1
// check (#297). "artifacts" must be digested ArtifactPointer objects — a model
// cannot reliably self-report a content digest, and no harness step resolves a
// model-declared path into one, so a model that fills the field produces an
// invalid completion (#301); stage evidence (e.g. a reviewer's diff) and the
// captured transcript are recorded and digested by the runner, never
// self-reported here. These fields are described conditionally/out-of-band
// instead of shown inline.
const resultShapeHint = `{"status": "success"|"failure"|"blocked", "outputs": {...}, "summary": "...", "metrics": {...}}

On a "failure" or "blocked" status, also include an "error" object: {"code": "...", "message": "..."} (both non-empty). Omit "error" entirely on success. Do not populate "artifacts" or "transcript" — the runner records and digests them.

On a "blocked" status, if you can name specific blocking issue numbers, set outputs.blockedBy to a single comma-separated string (e.g. "441,442") — never an array or object; outputs accepts scalars only and a structured value is schema-rejected.`

// verdictShapeHint shows finding.severity as an explicit enum: the schema's
// finding is additionalProperties:false with severity ∈
// {info,warning,error,critical}, so an unconstrained "severity": "..." let a
// model guess an out-of-enum value ("Medium") and add extra per-finding fields,
// failing validation with no journaled explanation (#304, same shape-gap class
// as #297). A finding carries only severity/message/location — evidence is a
// separate top-level array of artifact pointers, not per-finding.
const verdictShapeHint = `{"decision": "pass"|"fail"|"needs-changes", "rationale": "...", "findings": [{"severity": "info"|"warning"|"error"|"critical", "message": "...", "location": "..."}], "summary": "..."}`
