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

const resultShapeHint = `{"status": "success"|"failure"|"blocked", "outputs": {...}, "artifacts": [...], "summary": "...", "metrics": {...}, "error": {"code": "...", "message": "..."}}`

const verdictShapeHint = `{"decision": "pass"|"fail"|"needs-changes", "rationale": "...", "findings": [{"severity": "...", "message": "...", "location": "..."}], "summary": "..."}`
