package harness

import (
	"strings"

	"github.com/goobers/goobers/internal/mcpio"
)

// Only launcher-supplied access expands the built-in server's ordinary tools.
func goobersIOToolsFor(req RunRequest) []string {
	return append(append([]string(nil), goobersIOTools...), mcpio.ChildWorkflowToolNames(req.ChildWorkflows, req.Envelope.RunID)...)
}

func prefixedGoobersIOTools(req RunRequest, prefix string) []string {
	names := goobersIOToolsFor(req)
	for i := range names {
		names[i] = prefix + names[i]
	}
	return names
}

func writeChildWorkflowPrompt(b *strings.Builder, req RunRequest) {
	if len(mcpio.ChildWorkflowToolNames(req.ChildWorkflows, req.Envelope.RunID)) == 0 {
		return
	}
	b.WriteString("Child workflows are enabled for this stage. Write a valid Workflow DSL file inside this workspace, outside .goobers and .git, then call `validate_child_workflow` with `sourceFile` for advisory diagnostics. Call `start_child_workflow` with `sourceFile` and a stable `invocationKey` to accept a child. Reuse the same key and unchanged source when retrying an uncertain start; use `get_child_workflow` with that key to inspect custody. A queued receipt does not mean the child has run or completed. Status reads do not durably wait, retrieve result/workspace references, or merge child changes. The daemon supplies scope and authority; do not request or supply credentials, a different run, endpoint, or policy.\n\n")
	b.WriteString("After the child returns, choose `resolve_child_workflow` with its invocationKey, exact terminal resultRef, and action merge, replace, or discard. The request yields control to the runner; only verified application releases the child slot. An applied=false receipt is pending, not permission to modify the workspace during handoff. Discard does not undo provider effects such as an already published PR.\n\n")
}
