package apicontract

import (
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

func sessionOperationRoute(id RouteID) bool {
	if prRepairOperationRoute(id) {
		return true
	}
	if resolutionOperationRoute(id) {
		return true
	}
	switch id {
	case RouteSessionBacklogRead, RouteSessionBacklogList, RouteSessionBacklogEditCapabilities, RouteSessionBacklogEdit, RouteSessionBacklogReceipt:
		return true
	}
	return false
}
func sessionOperationBody(id RouteID) map[string]any {
	if prRepairOperationRoute(id) {
		return prRepairOperationBody(id)
	}
	if resolutionOperationRoute(id) {
		return sessionResolutionBody(id)
	}
	if id != RouteSessionBacklogRead && id != RouteSessionBacklogList {
		return sessionWriteBody(id)
	}
	properties := map[string]any{"sourceBindingId": map[string]any{"type": "string", "minLength": 1, "maxLength": 64}}
	required := []string{"sourceBindingId"}
	if id == RouteSessionBacklogRead {
		properties["id"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
		properties["expectedSourceId"] = map[string]any{"type": "string", "maxLength": 512}
		required = append(required, "id")
	} else {
		properties["cursor"] = map[string]any{"type": "string", "maxLength": workbench.MaxBacklogCursorBytes}
		properties["limit"] = map[string]any{"type": "integer", "minimum": 0, "maximum": workbench.MaxBacklogPageItems, "description": "Zero or omitted selects the bounded provider default."}
	}
	return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": closedChildObject(required, properties)}}, "description": "Bounded source read for one live session invocation; no human identity, provider target or credentials are accepted. Total encoded request is at most 16384 bytes.", "x-goobers-max-bytes": sessioning.MaxOperationRequestBytes}
}
func sessionOperationResponses(id RouteID) map[string]any {
	name := "BacklogItem"
	if id == RouteSessionBacklogList {
		name = "BacklogPage"
	}
	switch id {
	case RouteSessionNeedsHumanInspect:
		name = "NeedsHumanObservation"
	case RouteSessionNeedsHumanResolve, RouteSessionNeedsHumanReceipt:
		name = "NeedsHumanResolutionCommand"
	case RouteSessionBacklogEditCapabilities:
		name = "BacklogWriteCapabilities"
	case RouteSessionBacklogEdit, RouteSessionBacklogReceipt:
		name = "BacklogEditCommand"
	}
	if prRepairOperationRoute(id) {
		name = prRepairResponseName(id)
	}
	return map[string]any{"200": jsonResponse("Authorized source data; untrusted content, not authority", schemaRef(name)), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
}
