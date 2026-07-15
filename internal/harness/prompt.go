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

	completionKind, schemaHint := "result", resultShapeHint
	if req.Mode == ModeReview {
		completionKind, schemaHint = "verdict", verdictShapeHint
	}
	fmt.Fprintf(&b, "## Completion contract\n\n"+
		"When you are finished, write your %s as JSON to `%s` (relative to your "+
		"working directory), matching this shape:\n\n%s\n\n"+
		"Do not consider the task complete until this file exists and is valid JSON. "+
		"Never print credentials or tokens to stdout/stderr.\n",
		completionKind, req.CompletionPath, schemaHint)

	return b.String()
}

// resultShapeHint deliberately omits both "error" and "artifacts" from the base
// shape. "error" is required only on a "failure"/"blocked" status, and an empty
// error object on a successful result fails the schema's errorInfo minLength:1
// check (#297). "artifacts" must be digested ArtifactPointer objects — a model
// cannot reliably self-report a content digest, and no harness step resolves a
// model-declared path into one, so a model that fills the field produces an
// invalid completion (#301); stage evidence (e.g. a reviewer's diff) is recorded
// and digested by the runner, never self-reported here. Both fields are
// described conditionally/out-of-band instead of shown inline.
const resultShapeHint = `{"status": "success"|"failure"|"blocked", "outputs": {...}, "summary": "...", "metrics": {...}}

On a "failure" or "blocked" status, also include an "error" object: {"code": "...", "message": "..."} (both non-empty). Omit "error" entirely on success. Do not populate "artifacts" — the runner records and digests any stage artifacts.`

const verdictShapeHint = `{"decision": "pass"|"fail"|"needs-changes", "rationale": "...", "findings": [{"severity": "...", "message": "...", "location": "..."}], "summary": "..."}`
