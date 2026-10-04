package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
)

// ChildWorkflowGrantTokenPrefix mirrors podauth without an import cycle.
// TestChildWorkflowGrantPrefixMatchesPodauth pins the two trust domains together.
const ChildWorkflowGrantTokenPrefix = "goobers-child."

// ChildWorkflowService must verify grant cryptography/expiry and current runtime
// authority on EVERY call, including anonymous loopback requests. The route's
// bearer-prefix check is not authentication. Implementations bind the path run to
// the grant, pin occurrence/attempt/config/policy, and recheck before acceptance.
// Refusals use InterventionError; validation findings belong in the advisory DTO.
// No method may accept policy, origin or actor from authored source as authority.
type ChildWorkflowService interface {
	ValidateChildWorkflow(ctx context.Context, grant, run string, source []byte) (apicontract.ChildWorkflowValidationResponse, error)
	StartChildWorkflow(ctx context.Context, grant, run, invocationKey string, source []byte) (apicontract.ChildWorkflowResponse, error)
	ChildWorkflowStatus(ctx context.Context, grant, run, invocationKey string) (apicontract.ChildWorkflowResponse, error)
}

// WithChildWorkflowService enables the stage-only child workflow transport.
func WithChildWorkflowService(service ChildWorkflowService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API child workflow service is required")
		}
		config.childWorkflows = service
		return nil
	}
}

func registerChildWorkflowRoutes(router *Router, service ChildWorkflowService, errorLog *log.Logger) {
	for _, id := range []apicontract.RouteID{apicontract.RouteChildWorkflowValidate, apicontract.RouteChildWorkflowStart, apicontract.RouteChildWorkflowStatus} {
		router.Handle(id, childWorkflowHandler(id, service, errorLog))
	}
}

func childWorkflowHandler(id apicontract.RouteID, service ChildWorkflowService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "child_workflows_unavailable", "child workflows are not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		grant, ok := childWorkflowGrantFromRequest(request)
		if !ok {
			writeError(w, http.StatusForbidden, "child_workflow_requires_grant", "child workflows accept only a stage child-workflow grant")
			return
		}
		run := request.PathValue("run")
		if !apiv1.ValidRunID(run) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "run id is not a safe path segment")
			return
		}
		response, status, err := callChildWorkflow(request, id, service, grant, run)
		if err != nil {
			writePlaneError(w, errorLog, "child workflow operation", err)
			return
		}
		writeJSON(w, status, response)
	}
}

func callChildWorkflow(request *http.Request, id apicontract.RouteID, service ChildWorkflowService, grant, run string) (any, int, error) {
	if id == apicontract.RouteChildWorkflowStatus {
		key, err := childStringBody(request, "invocationKey", apicontract.MaxChildWorkflowInvocationKeyBytes)
		if err != nil || !validChildInvocationKey(key) {
			return nil, 0, invalidChildRequest("invocationKey must be a nonempty bounded key without whitespace padding or control characters")
		}
		result, err := service.ChildWorkflowStatus(request.Context(), grant, run, key)
		return result, http.StatusOK, err
	}
	key := ""
	if id == apicontract.RouteChildWorkflowStart {
		values := request.Header.Values(HeaderIdempotencyKey)
		if len(values) != 1 || !validChildInvocationKey(values[0]) {
			return nil, 0, invalidChildRequest("one Idempotency-Key header is required, up to 256 bytes without whitespace padding or control characters")
		}
		key = values[0]
	}
	source, err := childStringBody(request, "source", apicontract.MaxChildWorkflowSourceBytes)
	if err != nil {
		return nil, 0, invalidChildRequest("body must contain only source, a nonempty UTF-8 string of at most 1048576 bytes")
	}
	if id == apicontract.RouteChildWorkflowValidate {
		result, err := service.ValidateChildWorkflow(request.Context(), grant, run, []byte(source))
		return result, http.StatusOK, err
	}
	result, err := service.StartChildWorkflow(request.Context(), grant, run, key, []byte(source))
	return result, http.StatusAccepted, err
}

func invalidChildRequest(message string) error {
	return &InterventionError{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: message}
}

func validChildInvocationKey(key string) bool {
	return key != "" && len(key) <= apicontract.MaxChildWorkflowInvocationKeyBytes && utf8.ValidString(key) &&
		strings.TrimSpace(key) == key && strings.IndexFunc(key, unicode.IsControl) == -1
}

func childWorkflowGrantFromRequest(request *http.Request) (string, bool) {
	if principal, established := PrincipalFromRequest(request); established && principal.Issuer != ChildWorkflowPrincipalIssuer {
		return "", false
	}
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	const scheme = "Bearer "
	value := values[0]
	if len(value) <= len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(value[len(scheme):])
	return token, strings.HasPrefix(token, ChildWorkflowGrantTokenPrefix) && len(token) <= 4096
}

// One exact string field rejects unknown, duplicate and case-folded authority
// fields. The encoded body permits JSON's six-byte escapes but remains bounded.
func childStringBody(request *http.Request, field string, maxBytes int) (string, error) {
	defer func() { _ = request.Body.Close() }()
	limit := int64(6*maxBytes + 128)
	raw, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil || int64(len(raw)) > limit || !utf8.Valid(raw) {
		return "", errors.New("invalid bounded JSON body")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return "", errors.New("JSON object required")
	}
	key, err := decoder.Token()
	if err != nil || key != field {
		return "", errors.New("exact field required")
	}
	var value string
	if err := decoder.Decode(&value); err != nil || value == "" || len(value) > maxBytes {
		return "", errors.New("bounded nonempty string required")
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return "", errors.New("one field required")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("one JSON object required")
	}
	return value, nil
}
