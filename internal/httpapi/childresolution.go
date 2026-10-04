package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
)

func callChildResolution(request *http.Request, service ChildWorkflowService, grant, run string) (any, int, error) {
	resolver, ok := service.(ChildWorkflowResolutionService)
	if !ok {
		return nil, 0, NewInterventionError(http.StatusServiceUnavailable, "child_resolution_unavailable", "child result resolution is unavailable", nil)
	}
	body, err := childResolutionBody(request)
	if err != nil {
		return nil, 0, invalidChildRequest("body must contain only invocationKey, action (merge, replace or discard), and the expected resultRef digest")
	}
	result, err := resolver.ResolveChildWorkflow(request.Context(), grant, run, body)
	return result, http.StatusAccepted, err
}

func childResolutionBody(request *http.Request) (apicontract.ChildWorkflowResolveRequest, error) {
	defer func() { _ = request.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(request.Body, 8193))
	if err != nil || len(raw) > 8192 || !utf8.Valid(raw) {
		return apicontract.ChildWorkflowResolveRequest{}, errors.New("invalid resolution body")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return apicontract.ChildWorkflowResolveRequest{}, errors.New("object required")
	}
	fields := map[string]string{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return apicontract.ChildWorkflowResolveRequest{}, err
		}
		name, ok := key.(string)
		if !ok || (name != "invocationKey" && name != "action" && name != "resultRef") {
			return apicontract.ChildWorkflowResolveRequest{}, errors.New("unknown field")
		}
		if _, ok := fields[name]; ok {
			return apicontract.ChildWorkflowResolveRequest{}, errors.New("duplicate field")
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return apicontract.ChildWorkflowResolveRequest{}, err
		}
		fields[name] = value
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return apicontract.ChildWorkflowResolveRequest{}, errors.New("object required")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return apicontract.ChildWorkflowResolveRequest{}, errors.New("one object required")
	}
	result := apicontract.ChildWorkflowResolveRequest{InvocationKey: fields["invocationKey"], Action: fields["action"], ResultRef: fields["resultRef"]}
	if !validChildInvocationKey(result.InvocationKey) || !blobstore.ValidDigest(result.ResultRef) || (result.Action != "merge" && result.Action != "replace" && result.Action != "discard") {
		return result, errors.New("invalid resolution")
	}
	return result, nil
}
