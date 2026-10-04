package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/sessioning"
)

// InteractiveSessionService enforces current per-gaggle human access for every
// read and command. Acceptance commits before returning and is independent of
// browser lifetime. Run execution belongs to the daemon's durable queue worker.
type InteractiveSessionService interface {
	Create(context.Context, Principal, string, sessioning.CreateRequest) (sessioning.Acceptance, error)
	Get(context.Context, Principal, string, string) (sessioning.Session, error)
	List(context.Context, Principal, string, string, int) (sessioning.SessionPage, error)
	Messages(context.Context, Principal, string, string, uint64, int) (sessioning.MessagePage, error)
	SubmitMessage(context.Context, Principal, string, string, sessioning.MessageRequest) (sessioning.Acceptance, error)
	Close(context.Context, Principal, string, string, sessioning.CloseRequest) (sessioning.Acceptance, error)
}

// WithInteractiveSessions installs the shared human conversation service.
func WithInteractiveSessions(service InteractiveSessionService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("interactive session service is required")
		}
		config.interactiveSessions = service
		return nil
	}
}

func registerSessionRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	for _, pair := range [][2]apicontract.RouteID{{apicontract.RouteSessionList, apicontract.RouteSessionCreate}, {apicontract.RouteSessionMessages, apicontract.RouteSessionMessage}} {
		router.HandleByMethod(map[string]apicontract.RouteID{http.MethodGet: pair[0], http.MethodPost: pair[1]}, map[apicontract.RouteID]http.HandlerFunc{pair[0]: sessionHandler(pair[0], config.interactiveSessions, errorLog), pair[1]: sessionHandler(pair[1], config.interactiveSessions, errorLog)})
	}
	for _, id := range []apicontract.RouteID{apicontract.RouteSessionGet, apicontract.RouteSessionClose} {
		router.Handle(id, sessionHandler(id, config.interactiveSessions, errorLog))
	}
}

func sessionHandler(id apicontract.RouteID, service InteractiveSessionService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		principal, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if r.Method == http.MethodPost && !principal.HasRole(RoleOperate) {
			writeError(w, http.StatusForbidden, "interactive_access_denied", "Session commands require operator access.")
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "interactive_unavailable", "Shared sessions are unavailable on this server.")
			return
		}
		var result any
		var err error
		if r.Method == http.MethodGet {
			result, err = readSession(r, principal, service, id)
		} else {
			key, valid := sessionMutationKey(w, r)
			if !valid {
				return
			}
			result, err = writeSession(r, principal, service, id, key)
		}
		if err != nil {
			writePlaneError(w, errorLog, "interactive session", err)
			return
		}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusAccepted
		}
		writeJSON(w, status, result)
	}
}

func sessionMutationKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	if err := validateInteractiveTransport(r); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return "", false
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Session commands do not accept query parameters.")
		return "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 400<<10)
	return requireIdempotencyKey(w, r)
}

func sessionBadRequest(message string) error {
	return &InterventionError{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: message}
}

func readSession(r *http.Request, p Principal, service InteractiveSessionService, id apicontract.RouteID) (any, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, sessionBadRequest("Invalid session pagination.")
	}
	limit, err := sessionPageLimit(query, id)
	if err != nil {
		return nil, err
	}
	gaggle, session := r.PathValue("gaggle"), r.PathValue("session")
	switch id {
	case apicontract.RouteSessionList:
		return service.List(r.Context(), p, gaggle, query.Get("cursor"), limit)
	case apicontract.RouteSessionMessages:
		var after uint64
		if value := query.Get("after"); value != "" {
			after, err = strconv.ParseUint(value, 10, 64)
			if err != nil || after > 1<<53-1 {
				return nil, sessionBadRequest("Invalid message cursor.")
			}
		}
		return service.Messages(r.Context(), p, gaggle, session, after, limit)
	default:
		return service.Get(r.Context(), p, gaggle, session)
	}
}

func sessionPageLimit(query url.Values, id apicontract.RouteID) (int, error) {
	for key, values := range query {
		allowed := (key == "limit" && id != apicontract.RouteSessionGet) || (key == "cursor" && id == apicontract.RouteSessionList) || (key == "after" && id == apicontract.RouteSessionMessages)
		if !allowed || len(values) != 1 || len(values[0]) > 200 || values[0] == "" {
			return 0, sessionBadRequest("Invalid session pagination.")
		}
	}
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > sessioning.MaxPageSize {
			return 0, sessionBadRequest("Session page limit must be between 1 and 200.")
		}
		limit = parsed
	}
	return limit, nil
}

func writeSession(r *http.Request, p Principal, service InteractiveSessionService, id apicontract.RouteID, key string) (any, error) {
	gaggle, session := r.PathValue("gaggle"), r.PathValue("session")
	switch id {
	case apicontract.RouteSessionCreate:
		var input apicontract.SessionCreateRequest
		if err := decodeWriteRequestBounded(r, &input, (400<<10)+1); err != nil || !sessionText(input.Title, sessioning.MaxTitleBytes, true) || !sessionText(input.Goober, 128, true) {
			return nil, sessionBadRequest("Provide a title and configured Goober within the allowed length.")
		}
		return service.Create(r.Context(), p, gaggle, sessioning.CreateRequest{RequestID: key, Title: input.Title, Goober: input.Goober})
	case apicontract.RouteSessionMessage:
		var input apicontract.SessionMessageRequest
		if err := decodeWriteRequestBounded(r, &input, (400<<10)+1); err != nil || !sessionText(input.Text, sessioning.MaxTextBytes, true) || sessioning.ValidatePRRepairTarget(input.RepairTarget) != nil {
			return nil, sessionBadRequest("Provide a message of at most 64 KiB and a valid optional PR selection.")
		}
		return service.SubmitMessage(r.Context(), p, gaggle, session, sessioning.MessageRequest{RequestID: key, Text: input.Text, RepairTarget: input.RepairTarget})
	default:
		var input *apicontract.SessionCloseRequest
		if err := decodeWriteRequestBounded(r, &input, (400<<10)+1); err != nil || input == nil || !sessionText(input.Reason, 4096, false) {
			return nil, sessionBadRequest("Provide a close reason of at most 4 KiB.")
		}
		return service.Close(r.Context(), p, gaggle, session, sessioning.CloseRequest{RequestID: key, Reason: input.Reason})
	}
}

func sessionText(value string, max int, required bool) bool {
	return utf8.ValidString(value) && len(value) <= max && !strings.ContainsRune(value, 0) && (!required || strings.TrimSpace(value) != "")
}
