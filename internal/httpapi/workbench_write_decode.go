package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

const maxWorkbenchPatchBytes = 256 << 10

func decodeWorkbenchPatch(r *http.Request) (workbench.BacklogPatchRequest, error) {
	defer func() { _ = r.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWorkbenchPatchBytes+1))
	invalid := sessionBadRequest("Provide one bounded native field change with sourceId and expectedRevision.")
	if err != nil || len(raw) > maxWorkbenchPatchBytes || !utf8.Valid(raw) || !workbenchNativeID(r.PathValue("item")) {
		return workbench.BacklogPatchRequest{}, invalid
	}
	if !closedWorkbenchPatch(raw) {
		return workbench.BacklogPatchRequest{}, invalid
	}
	var input apicontract.BacklogPatchInput
	if json.Unmarshal(raw, &input) != nil || !sessionText(input.SourceID, 512, true) || !sessionText(input.ExpectedRevision, 512, true) {
		return workbench.BacklogPatchRequest{}, invalid
	}
	request := workbench.BacklogPatchRequest{ID: r.PathValue("item"), SourceID: input.SourceID, ExpectedRevision: input.ExpectedRevision, Field: input.Field, Value: input.Value, Values: input.Values}
	if providers.ValidateNativeWorkItemPatch(providers.NativeWorkItemPatch{ID: request.ID, StableID: request.SourceID, ExpectedRevision: request.ExpectedRevision, Field: string(request.Field), Value: request.Value, Values: request.Values}, "") != nil {
		return workbench.BacklogPatchRequest{}, invalid
	}
	return request, nil
}
func closedWorkbenchPatch(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] || !workbenchPatchField(name) {
			return false
		}
		seen[name] = true
		var field json.RawMessage
		if decoder.Decode(&field) != nil || bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return false
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF {
		return false
	}
	return seen["sourceId"] && seen["expectedRevision"] && seen["field"] && seen["value"] != seen["values"]
}
func workbenchPatchField(name string) bool {
	switch name {
	case "sourceId", "expectedRevision", "field", "value", "values":
		return true
	default:
		return false
	}
}
