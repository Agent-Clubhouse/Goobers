package sessioning

import (
	"bytes"
	"encoding/json"
	"io"
)

// DecodeMessageContent accepts exactly human content and optional selection.
// It rejects duplicate/case-aliased fields at each level before typed decoding.
func DecodeMessageContent(raw []byte) (MessageRequest, error) {
	var request MessageRequest
	if len(raw) > 400<<10 {
		return request, ErrInvalidRequest
	}
	fields, err := repairJSONFields(raw, "text", "repairTarget")
	if err != nil {
		return request, err
	}
	if err = json.Unmarshal(fields["text"], &request.Text); err != nil {
		return request, ErrInvalidRequest
	}
	if selection, ok := fields["repairTarget"]; ok {
		request.RepairTarget, err = decodeRepairTarget(selection)
	}
	return request, err
}

func decodeRepairTarget(raw []byte) (*PRRepairTarget, error) {
	if len(raw) > MaxRepairTargetBytes {
		return nil, ErrInvalidRequest
	}
	fields, err := repairJSONFields(raw, "sourceBindingId", "repository", "repositorySourceId", "id", "sourceId", "expectedHeadSha")
	if err != nil {
		return nil, err
	}
	if _, err = repairJSONFields(fields["repository"], "provider", "owner", "project", "name"); err != nil {
		return nil, err
	}
	var result PRRepairTarget
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, ErrInvalidRequest
	}
	if err = ValidatePRRepairTarget(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func repairJSONFields(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrInvalidRequest
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, ErrInvalidRequest
		}
		name, ok := key.(string)
		if !ok || !repairJSONName(name, allowed) || fields[name] != nil {
			return nil, ErrInvalidRequest
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrInvalidRequest
		}
		fields[name] = value
	}
	if close, err := decoder.Token(); err != nil || close != json.Delim('}') {
		return nil, ErrInvalidRequest
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, ErrInvalidRequest
	}
	return fields, nil
}
func repairJSONName(name string, allowed []string) bool {
	for _, candidate := range allowed {
		if name == candidate {
			return true
		}
	}
	return false
}
