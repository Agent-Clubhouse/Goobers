package apicontract

import (
	"net/http"

	"github.com/goobers/goobers/internal/sessioning"
)

func sessionParameters(id RouteID) []map[string]any {
	if id != RouteSessionList && id != RouteSessionMessages {
		return nil
	}
	parameters := []map[string]any{{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": sessioning.MaxPageSize, "default": 50}}}
	cursor, schema := "cursor", map[string]any{"type": "string", "maxLength": 200, "minLength": 1}
	if id == RouteSessionMessages {
		cursor, schema = "after", map[string]any{"type": "integer", "minimum": 0, "maximum": 9007199254740991}
	}
	return append(parameters, map[string]any{"name": cursor, "in": "query", "schema": schema})
}

func sessionString(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
func sessionArray(name string) map[string]any {
	return map[string]any{"type": "array", "maxItems": sessioning.MaxPageSize, "items": schemaRef(name)}
}

func openAPISessionSchemas() map[string]any {
	return map[string]any{
		"SessionCreateRequest":  closedChildObject([]string{"title", "goober"}, map[string]any{"title": sessionString(sessioning.MaxTitleBytes), "goober": sessionString(128)}),
		"SessionMessageRequest": closedChildObject([]string{"text"}, map[string]any{"text": sessionString(sessioning.MaxTextBytes)}),
		"SessionCloseRequest":   closedChildObject([]string{"reason"}, map[string]any{"reason": sessionString(4096)}),
		"SessionActor":          closedChildObject([]string{"issuer", "subject"}, map[string]any{"issuer": stringSchema(), "subject": stringSchema()}),
		"InteractiveSession": closedChildObject([]string{"id", "gaggle", "title", "goober", "configGeneration", "gooberDigest", "state", "createdBy", "createdAt", "updatedAt", "nextSequence"}, map[string]any{
			"id": stringSchema(), "gaggle": stringSchema(), "title": sessionString(sessioning.MaxTitleBytes), "goober": stringSchema(), "configGeneration": stringSchema(), "gooberDigest": stringSchema(),
			"state": map[string]any{"type": "string", "enum": []string{"idle", "queued", "running", "cancel-requested", "closed"}}, "createdBy": schemaRef("SessionActor"),
			"createdAt": dateTimeSchema(), "updatedAt": dateTimeSchema(), "nextSequence": map[string]any{"type": "integer", "minimum": 1}, "activeTurnId": stringSchema(), "lastOutcome": stringSchema(),
		}),
		"SessionMessage": closedChildObject([]string{"id", "sessionId", "sequence", "actorKind", "text", "createdAt"}, map[string]any{
			"id": stringSchema(), "sessionId": stringSchema(), "sequence": map[string]any{"type": "integer", "minimum": 1}, "actorKind": map[string]any{"type": "string", "enum": []string{"human", "agent", "system"}}, "actor": schemaRef("SessionActor"),
			"text": sessionString(sessioning.MaxTextBytes), "createdAt": dateTimeSchema(), "turnId": stringSchema(), "runId": stringSchema(), "outcome": stringSchema(),
		}),
		"SessionAcceptance":  closedChildObject([]string{"session", "duplicate"}, map[string]any{"session": schemaRef("InteractiveSession"), "message": schemaRef("SessionMessage"), "acceptanceId": stringSchema(), "duplicate": map[string]any{"type": "boolean"}}),
		"SessionPage":        closedChildObject([]string{"items"}, map[string]any{"items": sessionArray("InteractiveSession"), "nextCursor": stringSchema()}),
		"SessionMessagePage": closedChildObject([]string{"items"}, map[string]any{"items": sessionArray("SessionMessage"), "nextCursor": map[string]any{"type": "integer", "minimum": 1}}),
	}
}

func sessionResponses(route Route) map[string]any {
	name := "SessionAcceptance"
	switch route.ID {
	case RouteSessionList:
		name = "SessionPage"
	case RouteSessionGet:
		name = "InteractiveSession"
	case RouteSessionMessages:
		name = "SessionMessagePage"
	}
	code, description := "200", "Current authorized shared session data"
	if route.Method == http.MethodPost {
		code, description = "202", "Durable command accepted; execution or cancellation may remain pending"
	}
	return map[string]any{code: jsonResponse(description, schemaRef(name)), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
}
