package mcpio

import "github.com/goobers/goobers/internal/apicontract/childworkflowwire"

func (s *Server) toolDefs() []toolDef {
	definitions := toolDefs()
	if len(ChildWorkflowToolNames(s.tools.cfg.ChildWorkflows, s.tools.cfg.RunID)) == 0 {
		return definitions
	}
	return append(definitions, childWorkflowToolDefs()...)
}

func childWorkflowToolDefs() []toolDef {
	source := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 4096, "description": "Workspace-relative regular UTF-8 Workflow DSL file, at most 1 MiB, outside .goobers and .git."}
	key := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": childworkflowwire.MaxChildWorkflowInvocationKeyBytes, "description": "Stable invocation key within this parent occurrence. Reuse the same key and exact source for retries."}
	return []toolDef{
		{Name: "validate_child_workflow", Description: "Validate a generated Workflow against this stage's pinned policy. Advisory only; creates no child and does not start execution.", InputSchema: childToolSchema([]string{"sourceFile"}, map[string]interface{}{"sourceFile": source})},
		{Name: "start_child_workflow", Description: "Validate and accept one child workflow into durable custody. A queued receipt is not execution completion. Retry with the same invocationKey and unchanged source; get_child_workflow checks existing custody.", InputSchema: childToolSchema([]string{"sourceFile", "invocationKey"}, map[string]interface{}{"sourceFile": source, "invocationKey": key})},
		{Name: "get_child_workflow", Description: "Read one child invocation owned by this parent stage occurrence. This is a bounded status read; it does not await completion, retrieve referenced artifacts, or merge child work.", InputSchema: childToolSchema([]string{"invocationKey"}, map[string]interface{}{"invocationKey": key})},
		{Name: "resolve_child_workflow", Description: "Choose merge, replace or discard for this occurrence's exact terminal child result. This accepts durable intent; the runner must stop parent writers, verify application and then resume. Do not treat applied=false as completed. Discard does not undo an already published PR.", InputSchema: childToolSchema([]string{"invocationKey", "action", "resultRef"}, map[string]interface{}{"invocationKey": key, "action": map[string]interface{}{"type": "string", "enum": []string{"merge", "replace", "discard"}}, "resultRef": map[string]interface{}{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$", "description": "Exact resultRef from get_child_workflow for this terminal child."}})},
	}
}

func childToolSchema(required []string, properties map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "object", "required": required, "properties": properties, "additionalProperties": false}
}
