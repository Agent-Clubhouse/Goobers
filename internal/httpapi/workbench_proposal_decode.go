package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/workbench"
)

const maxMetadataRequestBytes = 2 << 20

func decodeMetadataChange(r *http.Request) (workbench.MetadataChangeRequest, error) {
	defer func() { _ = r.Body.Close() }()
	invalid := sessionBadRequest("Provide one bounded metadata edit with exact source revision.")
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxMetadataRequestBytes+1))
	if err != nil || len(raw) > maxMetadataRequestBytes || !utf8.Valid(raw) {
		return workbench.MetadataChangeRequest{}, invalid
	}
	fields, ok := metadataJSONObject(raw, []string{"path", "expected", "field", "value", "relationship", "objective", "alias"}, []string{"path", "expected"})
	if !ok || !closedMetadataExpected(fields["expected"]) {
		return workbench.MetadataChangeRequest{}, invalid
	}
	if !closedMetadataOperations(fields) {
		return workbench.MetadataChangeRequest{}, invalid
	}
	var request workbench.MetadataChangeRequest
	if json.Unmarshal(raw, &request) != nil || !validMetadataShape(r, request) {
		return workbench.MetadataChangeRequest{}, invalid
	}
	return request, nil
}
func validMetadataShape(r *http.Request, input workbench.MetadataChangeRequest) bool {
	bindings := map[string]bool{r.PathValue("source"): true}
	if input.Relationship != nil {
		bindings[input.Relationship.Edge.From.SourceBindingID] = true
		bindings[input.Relationship.Edge.To.SourceBindingID] = true
	}
	if input.Alias != nil {
		bindings[input.Alias.Alias.Target.SourceBindingID] = true
	}
	return workbench.ValidateMetadataChangeRequest(workbench.Scope{GaggleID: r.PathValue("gaggle"), Bindings: bindings}, r.PathValue("source"), input) == nil
}
func closedMetadataOperations(fields map[string]json.RawMessage) bool {
	for _, entry := range []struct {
		name     string
		validate func([]byte) bool
	}{{"relationship", closedMetadataRelationship}, {"objective", closedMetadataObjective}, {"alias", closedMetadataAlias}} {
		if raw, ok := fields[entry.name]; ok && !entry.validate(raw) {
			return false
		}
	}
	return true
}
func closedMetadataObjective(raw []byte) bool {
	_, ok := metadataJSONObject(raw, []string{"objectiveId", "title"}, []string{"objectiveId", "title"})
	return ok
}
func closedMetadataAlias(raw []byte) bool {
	edit, ok := metadataJSONObject(raw, []string{"action", "alias"}, []string{"action", "alias"})
	if !ok {
		return false
	}
	alias, ok := metadataJSONObject(edit["alias"], []string{"name", "target"}, []string{"name", "target"})
	if !ok {
		return false
	}
	_, ok = metadataJSONObject(alias["target"], []string{"gaggleId", "sourceBindingId", "kind", "sourceId"}, []string{"gaggleId", "sourceBindingId", "kind", "sourceId"})
	return ok
}
func closedMetadataExpected(raw []byte) bool {
	_, ok := metadataJSONObject(raw, []string{"commit", "blobId", "contentDigest"}, []string{"commit", "blobId", "contentDigest"})
	return ok
}
func closedMetadataRelationship(raw []byte) bool {
	edit, ok := metadataJSONObject(raw, []string{"action", "edge"}, []string{"action", "edge"})
	if !ok {
		return false
	}
	edge, ok := metadataJSONObject(edit["edge"], []string{"edgeId", "kind", "from", "to", "rationale"}, []string{"edgeId", "kind", "from", "to"})
	if !ok {
		return false
	}
	for _, name := range []string{"from", "to"} {
		if _, ok := metadataJSONObject(edge[name], []string{"gaggleId", "sourceBindingId", "kind", "sourceId"}, []string{"gaggleId", "sourceBindingId", "kind", "sourceId"}); !ok {
			return false
		}
	}
	return true
}

// Exact keys at every level reject duplicate and case-folded identity fields.
func metadataJSONObject(raw []byte, allowed, required []string) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || fields[name] != nil || !slices.Contains(allowed, name) {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, false
		}
		fields[name] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, false
	}
	for _, name := range required {
		if fields[name] == nil {
			return nil, false
		}
	}
	return fields, true
}
func emptyMetadataCommand(r *http.Request) error {
	if err := validateInteractiveTransport(r); err != nil {
		return sessionBadRequest("Use application/json and a same-origin request.")
	}
	defer func() { _ = r.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 513))
	if err != nil || len(raw) > 512 {
		return sessionBadRequest("Provide an empty JSON object.")
	}
	if _, ok := metadataJSONObject(raw, nil, nil); !ok {
		return sessionBadRequest("Provide an empty JSON object.")
	}
	return nil
}
