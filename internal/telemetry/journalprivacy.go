package telemetry

import (
	"bytes"
	"encoding/json"
)

var redactedServiceHealthBody = []byte(`{"telemetryBodyRedacted":"invalid-service-health-json"}`)

// Journal collectors do not grant identity consent. Local service-health
// journals intentionally contain host/account names and recovery paths; export
// only their operational projection. Consented identity remains available on
// the independent diagnostic channel and destination resource attributes.
// Work from the already-scrubbed bytes, never the original in-memory Event.
// This runs on the export worker, not the journal's commit path.
func journalExportBody(body []byte) []byte {
	if !possibleServiceHealthBody(body) {
		return body
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return redactedServiceHealthBody
	}
	var kind string
	if json.Unmarshal(envelope["type"], &kind) != nil || kind != "service.health" {
		return body
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(envelope["runner"], &payload) != nil {
		return redactedServiceHealthBody
	}
	fields := selectHealthScalars(payload, []string{
		"schemaVersion", "observedAt", "instanceId", "instanceDisplayName", "identityProblem",
		"windowCoverage", "daemonStartedAt", "processUptimeSeconds", "observedUncleanRestarts", "observationWindowStart",
	})
	if raw := payload["recoveryInventory"]; len(raw) > 0 {
		var inventory map[string]json.RawMessage
		if json.Unmarshal(raw, &inventory) == nil {
			fields["recoveryInventory"], _ = json.Marshal(selectHealthScalars(inventory,
				[]string{"state", "used", "limit", "unreadable", "highWaterPercent", "earliestRetainUntil"}))
		}
	}
	// RawMessage preserves integer precision and previously applied redaction.
	// Values originate in successfully decoded JSON, so these marshals cannot
	// encounter an unsupported Go value or malformed RawMessage.
	envelope["runner"], _ = json.Marshal(fields)
	projected, err := json.Marshal(envelope)
	if err != nil {
		return redactedServiceHealthBody
	}
	return projected
}

func possibleServiceHealthBody(body []byte) bool {
	// Ordinary journal records remain byte-for-byte unchanged without decoding.
	// A Unicode escape may hide part of the event type; do not bypass projection
	// merely because an old/externally serialized journal escaped ASCII bytes.
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) &&
		(bytes.Contains(body, []byte("service.health")) || bytes.Contains(body, []byte(`\u`)))
}

func selectHealthScalars(source map[string]json.RawMessage, keys []string) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage)
	for _, key := range keys {
		value := bytes.TrimSpace(source[key])
		// A known scalar name must not become an arbitrary nested-payload route.
		if len(value) > 0 && value[0] != '{' && value[0] != '[' {
			result[key] = value
		}
	}
	return result
}
