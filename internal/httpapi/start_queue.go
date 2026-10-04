package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/apicontract"
)

// StartQueueService requires current per-gaggle authority at the service layer.
type StartQueueService interface {
	StartQueue(context.Context, Principal, string, string, int) (apicontract.StartQueuePage, error)
	StartQueueItem(context.Context, Principal, string, string) (apicontract.StartQueueItem, error)
	CancelQueuedStart(context.Context, Principal, string, string, apicontract.StartQueueCancelInput) (apicontract.StartQueueItem, error)
}

// WithStartQueue enables bounded human inspection and attributed cancellation.
func WithStartQueue(service StartQueueService) HandlerOption {
	return func(c *handlerConfig) error {
		if service == nil {
			return errors.New("start queue service required")
		}
		c.startQueue = service
		return nil
	}
}
func registerStartQueueRoutes(router *Router, c handlerConfig, errorLog *log.Logger) {
	for _, id := range []apicontract.RouteID{apicontract.RouteStartQueue, apicontract.RouteStartQueueItem, apicontract.RouteStartQueueCancel} {
		router.Handle(id, startQueueHandler(id, c.startQueue, errorLog))
	}
}
func startQueueHandler(id apicontract.RouteID, service StartQueueService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 || (id == apicontract.RouteStartQueueCancel && !p.HasRole(RoleOperate)) {
			writeError(w, http.StatusForbidden, "queue_access_denied", "Start queue access is not authorized.")
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "queue_unavailable", "Start queue controls are unavailable.")
			return
		}
		result, err := dispatchStartQueue(r, p, service, id)
		if err != nil {
			writePlaneError(w, errorLog, "start queue", err)
			return
		}
		status := http.StatusOK
		if id == apicontract.RouteStartQueueCancel {
			status = http.StatusAccepted
		}
		writeJSON(w, status, result)
	}
}
func dispatchStartQueue(r *http.Request, p Principal, s StartQueueService, id apicontract.RouteID) (any, error) {
	bad := func() (any, error) {
		return nil, NewInterventionError(http.StatusBadRequest, CodeInvalidRequest, "Invalid bounded start queue request.", nil)
	}
	gaggle := r.PathValue("gaggle")
	if gaggle == "" || len(gaggle) > 256 {
		return bad()
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return bad()
	}
	if id == apicontract.RouteStartQueue {
		for key, values := range query {
			if (key != "cursor" && key != "limit") || len(values) != 1 || values[0] == "" || len(values[0]) > 128 {
				return bad()
			}
		}
		limit := 25
		if query.Get("limit") != "" {
			limit, err = strconv.Atoi(query.Get("limit"))
			if err != nil || limit < 1 || limit > 50 {
				return bad()
			}
		}
		return s.StartQueue(r.Context(), p, gaggle, query.Get("cursor"), limit)
	}
	acceptance := r.PathValue("acceptance")
	if len(query) != 0 || len(acceptance) != 40 || !strings.HasPrefix(acceptance, "trigger-") || strings.TrimPrefix(acceptance, "trigger-") == "" || strings.Trim(acceptance[8:], "0123456789abcdef") != "" {
		return bad()
	}
	if id == apicontract.RouteStartQueueItem {
		return s.StartQueueItem(r.Context(), p, gaggle, acceptance)
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return bad()
	}
	input, err := decodeStartQueueCancellation(r.Body)
	if err != nil {
		return bad()
	}
	return s.CancelQueuedStart(r.Context(), p, gaggle, acceptance, input)
}
func decodeStartQueueCancellation(body io.ReadCloser) (apicontract.StartQueueCancelInput, error) {
	defer func() { _ = body.Close() }()
	var result apicontract.StartQueueCancelInput
	raw, err := io.ReadAll(io.LimitReader(body, 4097))
	if err != nil || len(raw) > 4096 || !utf8.Valid(raw) {
		return result, errors.New("invalid cancellation body")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return result, errors.New("object required")
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err = dec.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] || (key != "requestId" && key != "reason") {
			return result, errors.New("closed unique fields required")
		}
		seen[key] = true
		var value string
		if err = dec.Decode(&value); err != nil {
			return result, err
		}
		if key == "requestId" {
			result.RequestID = value
		} else {
			result.Reason = value
		}
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') || !seen["requestId"] || !seen["reason"] {
		return result, errors.New("required fields missing")
	}
	if err = dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return result, errors.New("one object required")
	}
	return result, nil
}
