package sessioning

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// DecodeBacklogRead rejects unknown, duplicate and case-folded fields before
// typed decoding. Tool arguments can never smuggle host authority into a call.
func DecodeBacklogRead(raw []byte) (BacklogReadRequest, error) {
	var r BacklogReadRequest
	if err := decodeOperation(raw, map[string]bool{"sourceBindingId": true, "id": true, "expectedSourceId": true}, &r); err != nil {
		return r, err
	}
	return r, ValidateBacklogRead(r)
}

// DecodeBacklogList shares the HTTP and MCP closed request decoder.
func DecodeBacklogList(raw []byte) (BacklogListRequest, error) {
	var r BacklogListRequest
	if err := decodeOperation(raw, map[string]bool{"sourceBindingId": true, "cursor": true, "limit": true}, &r); err != nil {
		return r, err
	}
	return r, ValidateBacklogList(r)
}
func decodeOperation(raw []byte, allowed map[string]bool, out any) error {
	invalid := errors.New("invalid bounded session operation arguments")
	if len(raw) > MaxOperationRequestBytes || !utf8.Valid(raw) {
		return invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return invalid
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return invalid
		}
		seen[name] = true
		var field json.RawMessage
		if decoder.Decode(&field) != nil || bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return invalid
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF {
		return invalid
	}
	if json.Unmarshal(raw, out) != nil {
		return invalid
	}
	return nil
}
