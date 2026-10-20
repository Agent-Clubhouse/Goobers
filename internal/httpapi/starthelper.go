package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/apicontract"
)

// HelperStartService compiles shorthand under the same authenticated stage grant.
// A server without this extension continues accepting authored Workflow source.
type HelperStartService interface {
	StartHelper(context.Context, string, string, string, apicontract.HelperRequest) (apicontract.ChildWorkflowResponse, error)
}

func callChildStart(request *http.Request, service ChildWorkflowService, grant, run, key string) (any, int, error) {
	source, helper, err := childStartBody(request)
	if err != nil {
		return nil, 0, invalidChildRequest("start body must contain exactly one bounded source or helper object with task, goober and optional harness/model/base")
	}
	if helper != nil {
		compiler, ok := service.(HelperStartService)
		if !ok {
			return nil, 0, NewInterventionError(http.StatusServiceUnavailable, "helper_start_unavailable", "helper shorthand is unavailable", nil)
		}
		result, err := compiler.StartHelper(request.Context(), grant, run, key, *helper)
		return result, http.StatusAccepted, err
	}
	result, err := service.StartChildWorkflow(request.Context(), grant, run, key, []byte(source))
	return result, http.StatusAccepted, err
}

func childStartBody(request *http.Request) (string, *apicontract.HelperRequest, error) {
	defer func() { _ = request.Body.Close() }()
	limit := int64(6*apicontract.MaxChildWorkflowSourceBytes + 128)
	raw, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil || int64(len(raw)) > limit || !utf8.Valid(raw) {
		return "", nil, errors.New("invalid start body")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return "", nil, errors.New("start object required")
	}
	field, err := decoder.Token()
	if err != nil {
		return "", nil, err
	}
	var source string
	var helper *apicontract.HelperRequest
	switch field {
	case "source":
		if err := decoder.Decode(&source); err != nil || source == "" || len(source) > apicontract.MaxChildWorkflowSourceBytes {
			return "", nil, errors.New("invalid source")
		}
	case "helper":
		helper, err = decodeHelperBody(decoder)
		if err != nil {
			return "", nil, err
		}
	default:
		return "", nil, errors.New("source or helper required")
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return "", nil, errors.New("one start field required")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", nil, errors.New("one start object required")
	}
	return source, helper, nil
}

func decodeHelperBody(decoder *json.Decoder) (*apicontract.HelperRequest, error) {
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("helper object required")
	}
	helper := &apicontract.HelperRequest{}
	fields := map[string]*string{"task": &helper.Task, "goober": &helper.Goober, "harness": &helper.Harness, "model": &helper.Model, "base": &helper.Base}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		field, ok := token.(string)
		if err != nil || !ok || fields[field] == nil || seen[field] {
			return nil, errors.New("unknown or duplicate helper field")
		}
		seen[field] = true
		if err := decoder.Decode(fields[field]); err != nil || *fields[field] == "" {
			return nil, errors.New("helper fields require nonempty strings")
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid helper object")
	}
	return helper, helper.Validate()
}
