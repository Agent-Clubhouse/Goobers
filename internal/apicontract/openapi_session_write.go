package apicontract

import "github.com/goobers/goobers/internal/sessioning"

func sessionWriteBody(id RouteID) map[string]any {
	properties := map[string]any{"sourceBindingId": sessionString(64)}
	required := []string{"sourceBindingId"}
	limit := sessioning.MaxOperationRequestBytes
	schema := closedChildObject(required, properties)
	switch id {
	case RouteSessionBacklogEdit:
		schema = openAPIWorkbenchWriteSchemas()["BacklogPatchInput"].(map[string]any)
		properties = schema["properties"].(map[string]any)
		properties["sourceBindingId"] = sessionString(64)
		properties["requestId"] = sessionString(128)
		properties["id"] = map[string]any{"type": "string", "pattern": "^[1-9][0-9]{0,18}$"}
		schema["required"] = []string{"sourceBindingId", "requestId", "id", "sourceId", "expectedRevision", "field"}
		limit = sessioning.MaxOperationWriteBytes
	case RouteSessionBacklogReceipt:
		properties["commandId"] = map[string]any{"type": "string", "pattern": "^workbench-[0-9a-f]{32}$"}
		schema["required"] = []string{"sourceBindingId", "commandId"}
	}
	return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema}}, "description": "One live session operation. Host binds actual human and turn; source data is not authority. Keep requestId unchanged after a lost reply; uncertain commands must not be replayed under new keys.", "x-goobers-max-bytes": limit}
}
