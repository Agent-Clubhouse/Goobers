package httpapi

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

// SessionOperationService authenticates its opaque live-turn token independently
// of human login. Reads repeat revocation checks and preserve human attribution.
type SessionOperationService interface {
	AuthenticateSessionOperation(string) (string, error)
	GetBacklogItem(context.Context, string, string, sessioning.BacklogReadRequest) (workbench.BacklogItem, error)
	ListBacklogItems(context.Context, string, string, sessioning.BacklogListRequest) (workbench.BacklogPage, error)
}

// WithSessionOperations installs the trusted per-invocation machine plane.
func WithSessionOperations(service SessionOperationService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("session operation service required")
		}
		config.sessionOperations = service
		return nil
	}
}

type sessionOperationAuthenticator struct {
	service SessionOperationService
	next    Authenticator
}

func (a sessionOperationAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	token, ok := sessionOperationToken(r)
	if !ok {
		return a.next.Authenticate(r)
	}
	if a.service == nil {
		return nil, errors.New("session operation service unavailable")
	}
	run, err := a.service.AuthenticateSessionOperation(token)
	if err != nil || !apiv1.ValidRunID(run) {
		return nil, errors.New("invalid or revoked session operation grant")
	}
	return &Principal{Issuer: sessioning.OperationIssuer, Subject: "run:" + run}, nil
}
func sessionOperationToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	value := values[0]
	if len(value) < 7 || !strings.EqualFold(value[:7], "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(value[7:])
	return token, strings.HasPrefix(token, sessioning.OperationTokenPrefix)
}
func authorizeSessionOperation(r *http.Request, p Principal) error {
	run, ok := strings.CutPrefix(p.Subject, "run:")
	prefix := strings.ReplaceAll(sessioning.OperationPath, "{run}", run) + "/"
	name, matched := strings.CutPrefix(r.URL.Path, prefix)
	if !ok || !apiv1.ValidRunID(run) || !matched || r.Method != http.MethodPost || !sessionOperationName(name) {
		return errors.New("session grant is confined to its own session operations")
	}
	return nil
}
func registerSessionOperationRoutes(router *Router, service SessionOperationService, errorLog *log.Logger) {
	for _, id := range []apicontract.RouteID{apicontract.RouteSessionBacklogRead, apicontract.RouteSessionBacklogList, apicontract.RouteSessionBacklogEditCapabilities, apicontract.RouteSessionBacklogEdit, apicontract.RouteSessionBacklogReceipt} {
		router.Handle(id, sessionOperationHandler(id, service, errorLog))
	}
}
func sessionOperationHandler(id apicontract.RouteID, service SessionOperationService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		if service == nil {
			writeError(w, 503, "session_operations_unavailable", "Session source operations are unavailable.")
			return
		}
		if status, code, message := validateMutationTransport(r); status != 0 {
			writeError(w, status, code, message)
			return
		}
		principal, ok := PrincipalFromRequest(r)
		token, hasToken := sessionOperationToken(r)
		if !ok || principal.Issuer != sessioning.OperationIssuer || !hasToken || authorizeSessionOperation(r, principal) != nil {
			writeError(w, 403, "session_operation_denied", "A live turn grant is required.")
			return
		}
		if r.URL.RawQuery != "" {
			writeError(w, 400, CodeInvalidRequest, "Query arguments are not allowed.")
			return
		}
		limit := sessioning.MaxOperationRequestBytes
		if id == apicontract.RouteSessionBacklogEdit {
			limit = sessioning.MaxOperationWriteBytes
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
		_ = r.Body.Close()
		if err != nil || len(raw) > limit {
			writeError(w, 400, CodeInvalidRequest, "Invalid bounded operation body.")
			return
		}
		result, err := callSessionOperation(r.Context(), service, id, token, r.PathValue("run"), raw)
		if err != nil {
			writePlaneError(w, errorLog, "session source operation", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}
func callSessionOperation(ctx context.Context, service SessionOperationService, id apicontract.RouteID, token, run string, raw []byte) (any, error) {
	if id == apicontract.RouteSessionBacklogRead {
		request, err := sessioning.DecodeBacklogRead(raw)
		if err != nil {
			return nil, sessionBadRequest("Invalid item read arguments.")
		}
		return service.GetBacklogItem(ctx, token, run, request)
	}
	if id != apicontract.RouteSessionBacklogList {
		return callSessionWrite(ctx, service, id, token, run, raw)
	}
	request, err := sessioning.DecodeBacklogList(raw)
	if err != nil {
		return nil, sessionBadRequest("Invalid list arguments.")
	}
	return service.ListBacklogItems(ctx, token, run, request)
}

// Invocation grants have disjoint operation sets and never inherit human roles.
func authorizeInvocationGrant(r *http.Request, p Principal) (bool, error) {
	switch p.Issuer {
	case sessioning.OperationIssuer:
		return true, authorizeSessionOperation(r, p)
	case ChildWorkflowPrincipalIssuer:
		return true, authorizeChildWorkflow(r, p)
	default:
		return false, nil
	}
}
