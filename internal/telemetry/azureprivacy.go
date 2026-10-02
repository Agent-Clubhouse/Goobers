package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Apply the journal health projection again at the HTTP boundary. Replay files
// may predate this fix and contain already-encoded, unprojected journal bodies.
// Never edit retained journal/spool files or change their stable record IDs.
func azureJournalHealthPayload(raw []byte) ([]byte, error) {
	if !bytes.Contains(raw, []byte("service.health")) && !bytes.Contains(raw, []byte(`\u`)) {
		return raw, nil
	}
	lines := bytes.Split(raw, []byte{'\n'})
	changed := false
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		projected, err := azureJournalHealthEnvelope(line)
		if err != nil {
			// A malformed candidate cannot fall through and leak its raw body.
			// Keep the error signature fixed and free of user data for suppression.
			return nil, errors.New("project Azure Monitor journal health: invalid envelope")
		}
		if !bytes.Equal(projected, line) {
			changed = true
			lines[i] = projected
		}
	}
	if !changed {
		return raw, nil
	}
	return bytes.Join(lines, []byte{'\n'}), nil
}

func azureJournalHealthEnvelope(line []byte) ([]byte, error) {
	var envelope, data, base map[string]json.RawMessage
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data["baseData"], &base); err != nil {
		return nil, err
	}
	if len(base["message"]) == 0 {
		return line, nil // Non-message telemetry in a mixed batch.
	}
	var message string
	if err := json.Unmarshal(base["message"], &message); err != nil {
		return nil, err
	}
	projected := journalExportBody([]byte(message))
	if bytes.Equal(projected, []byte(message)) {
		return line, nil
	}
	base["message"], _ = json.Marshal(string(projected))
	data["baseData"], _ = json.Marshal(base)
	envelope["data"], _ = json.Marshal(data)
	return json.Marshal(envelope)
}
