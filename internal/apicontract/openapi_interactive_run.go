package apicontract

func openAPIInteractiveRunSchemas() map[string]any {
	kinds := map[string]any{"type": "string", "enum": []string{"approve", "override", "deny", "guidance"}}
	sequence := map[string]any{"type": "integer", "minimum": 1}
	return map[string]any{
		"InteractiveRunView": closedChildObject([]string{"runId", "gaggle", "phase", "actions", "guidance", "restartReason"}, map[string]any{
			"runId": stringSchema(), "gaggle": stringSchema(), "phase": stringSchema(), "restartReason": stringSchema(),
			"actions": map[string]any{"type": "array", "items": schemaRef("InteractiveRunAction")}, "guidance": map[string]any{"type": "array", "maxItems": 100, "items": schemaRef("OperatorMessageRecord")},
		}),
		"InteractiveRunAction": closedChildObject([]string{"kind", "stage", "subjectSequence", "decisions", "available", "reason"}, map[string]any{
			"kind": kinds, "stage": stringSchema(), "subjectSequence": sequence, "decisions": map[string]any{"type": "array", "items": stringSchema()}, "available": map[string]any{"type": "boolean"}, "reason": stringSchema(),
		}),
		"InteractiveRunCommand": closedChildObject([]string{"kind", "stage", "expectedSubjectSequence"}, map[string]any{
			"kind": kinds, "stage": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "expectedSubjectSequence": sequence, "decision": stringSchema(), "rationale": map[string]any{"type": "string", "maxLength": 4096}, "guidance": map[string]any{"type": "string", "maxLength": 65536},
		}),
		"InteractiveRunCommandResult": closedChildObject([]string{"status", "accepted", "runId", "journalSequence", "phase"}, map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"applied", "saved", "pending", "failed"}}, "accepted": map[string]any{"type": "boolean"}, "runId": stringSchema(), "journalSequence": map[string]any{"type": "integer", "minimum": 0}, "phase": stringSchema(), "guidance": schemaRef("OperatorMessageRecord"),
		}),
	}
}
