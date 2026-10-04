package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

const maxMetadataRequestBytes = 2 << 20

func decodeMetadataChange(r *http.Request) (workbench.MetadataChangeRequest, error) {
	defer func() { _ = r.Body.Close() }()
	invalid := sessionBadRequest("Provide one bounded metadata edit with exact source revision.")
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxMetadataRequestBytes+1))
	if err != nil || len(raw) > maxMetadataRequestBytes || !utf8.Valid(raw) {
		return workbench.MetadataChangeRequest{}, invalid
	}
	fields, ok := metadataJSONObject(raw, []string{"path", "expected", "field", "value", "relationship"}, []string{"path", "expected"})
	if !ok || !closedMetadataExpected(fields["expected"]) {
		return workbench.MetadataChangeRequest{}, invalid
	}
	if edge, exists := fields["relationship"]; exists {
		if fields["field"] != nil || fields["value"] != nil || !closedMetadataRelationship(edge) {
			return workbench.MetadataChangeRequest{}, invalid
		}
	} else if fields["field"] == nil || fields["value"] == nil {
		return workbench.MetadataChangeRequest{}, invalid
	}
	var request workbench.MetadataChangeRequest
	if json.Unmarshal(raw, &request) != nil || !validMetadataShape(r, request) {
		return workbench.MetadataChangeRequest{}, invalid
	}
	return request, nil
}
func validMetadataShape(r *http.Request, input workbench.MetadataChangeRequest) bool {
	if !providers.ValidRepositorySourcePath(input.Path) || !providers.ValidSourceCommit(input.Expected.Commit) || !providers.ValidSourceCommit(input.Expected.BlobID) || !metadataDigestText(input.Expected.ContentDigest) {
		return false
	}
	if input.Relationship != nil {
		edit := input.Relationship
		if (edit.Action != "add" && edit.Action != "remove") || (edit.Edge.Kind != "references" && edit.Edge.Kind != "contributes-to") {
			return false
		}
		scope := workbench.Scope{GaggleID: r.PathValue("gaggle"), Bindings: map[string]bool{r.PathValue("source"): true, edit.Edge.From.SourceBindingID: true, edit.Edge.To.SourceBindingID: true}}
		return scope.ValidateEdge(edit.Edge) == nil
	}
	if input.Value == nil {
		return false
	}
	switch input.Field {
	case "title":
		return sessionText(*input.Value, 512, true)
	case "description":
		return len(*input.Value) <= workbench.MaxSourceBytes
	default:
		return false
	}
}
func metadataDigestText(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
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
