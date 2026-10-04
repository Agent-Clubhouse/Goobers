package apicontract

func openAPIEventIngressSchemas() map[string]any {
	return map[string]any{
		"GaggleEventEnvelope": closedChildObject([]string{"specversion", "id", "source", "type"}, map[string]any{"specversion": map[string]any{"type": "string", "const": "1.0"}, "id": sessionString(256), "source": sessionString(1024), "type": sessionString(256), "subject": sessionString(1024), "time": dateTimeSchema(), "dataschema": sessionString(1024), "datacontenttype": sessionString(1024), "data": map[string]any{}}),
		"GaggleEventDelivery": closedChildObject([]string{"consumer"}, map[string]any{"consumer": sessionString(128), "groupId": sessionString(128), "reason": sessionString(1024), "state": sessionString(32), "acceptanceId": sessionString(128), "runId": sessionString(128)}),
		"GaggleEventReceipt":  closedChildObject([]string{"receiptId", "gaggle", "binding", "source", "eventId", "digest", "acceptedAt", "duplicate", "state", "statusUrl", "tombstoned", "deliveries"}, map[string]any{"receiptId": sessionString(38), "gaggle": sessionString(253), "binding": sessionString(256), "source": sessionString(1024), "eventId": sessionString(256), "digest": sessionString(71), "acceptedAt": dateTimeSchema(), "duplicate": map[string]any{"type": "boolean"}, "state": workbenchEnum("routing_pending", "accepted_unmatched", "routing_failed", "routed", "routing_partial"), "statusUrl": sessionString(1024), "tombstoned": map[string]any{"type": "boolean"}, "deliveries": workbenchArray("GaggleEventDelivery", 32)}),
	}
}
func eventIngressResponses(id RouteID) map[string]any {
	code := "200"
	if id == RouteGaggleEventPublish {
		code = "202"
	}
	return map[string]any{code: jsonResponse("Durable event receipt; acceptance does not imply consumer completion", schemaRef("GaggleEventReceipt")), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
}
