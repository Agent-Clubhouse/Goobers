package apicontract

func openAPIStartQueueSchemas() map[string]any {
	return map[string]any{
		"StartQueueCancelInput":  closedChildObject([]string{"requestId", "reason"}, map[string]any{"requestId": sessionString(128), "reason": sessionString(512)}),
		"StartQueueCancellation": closedChildObject([]string{"requestId", "actor", "reason", "requestedAt", "state"}, map[string]any{"requestId": sessionString(128), "actor": sessionString(1024), "reason": sessionString(512), "requestedAt": dateTimeSchema(), "state": workbenchEnum("requested", "cancelled-before-dispatch", "confirmed", "already-terminal")}),
		"StartQueueItem":         closedChildObject([]string{"acceptanceId", "gaggle", "workflow", "source", "generation", "acceptedAt", "state"}, map[string]any{"acceptanceId": sessionString(40), "gaggle": sessionString(256), "workflow": sessionString(256), "source": workbenchEnum("manual", "schedule", "backlog", "event", "child", "session", "human-restart", "direct-engine", "signal", "legacy"), "generation": sessionString(256), "acceptedAt": dateTimeSchema(), "deadline": dateTimeSchema(), "state": workbenchEnum("accepted", "dispatching", "dispatched", "rejected"), "waitingReason": sessionString(256), "runId": sessionString(256), "disposition": workbenchEnum("cancelled", "expired"), "cancellation": schemaRef("StartQueueCancellation")}),
		"StartQueuePage":         closedChildObject([]string{"gaggle", "items"}, map[string]any{"gaggle": sessionString(256), "items": workbenchArray("StartQueueItem", 50), "nextCursor": sessionString(128)}),
	}
}
func startQueueResponses(id RouteID) map[string]any {
	name := "StartQueueItem"
	code := "200"
	if id == RouteStartQueue {
		name = "StartQueuePage"
	}
	if id == RouteStartQueueCancel {
		code = "202"
	}
	return map[string]any{code: jsonResponse("Current authorized durable start custody; a cancellation request is not termination", schemaRef(name)), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
}
