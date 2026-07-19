package httpapi

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

var identifierPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func registerInventoryRoutes(router *Router, reader readservice.Reader, errorLog *log.Logger) {
	router.Handle(apicontract.RouteInstance, func(w http.ResponseWriter, request *http.Request) {
		value, err := reader.Instance(request.Context())
		if err != nil {
			writeInventoryReadError(w, errorLog, "instance", err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	})

	router.Handle(apicontract.RouteGaggles, func(w http.ResponseWriter, request *http.Request) {
		page, ok := inventoryPageRequest(w, request)
		if !ok {
			return
		}
		value, err := reader.Gaggles(request.Context(), page)
		if err != nil {
			writeInventoryReadError(w, errorLog, "gaggles", err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	})

	router.Handle(apicontract.RouteGaggleGoobers, func(w http.ResponseWriter, request *http.Request) {
		gaggle := request.PathValue("gaggle")
		if !validIdentifier(gaggle) {
			writeError(w, http.StatusBadRequest, "invalid_identifier", "gaggle identifier is invalid")
			return
		}
		page, ok := inventoryPageRequest(w, request)
		if !ok {
			return
		}
		value, err := reader.Goobers(request.Context(), gaggle, page)
		if err != nil {
			writeInventoryReadError(w, errorLog, "goobers", err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	})

	router.Handle(apicontract.RouteGaggleWorkflows, func(w http.ResponseWriter, request *http.Request) {
		gaggle := request.PathValue("gaggle")
		if !validIdentifier(gaggle) {
			writeError(w, http.StatusBadRequest, "invalid_identifier", "gaggle identifier is invalid")
			return
		}
		page, ok := inventoryPageRequest(w, request)
		if !ok {
			return
		}
		value, err := reader.Workflows(request.Context(), gaggle, page)
		if err != nil {
			writeInventoryReadError(w, errorLog, "workflows", err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	})

	router.Handle(apicontract.RouteWorkflowDetail, func(w http.ResponseWriter, request *http.Request) {
		gaggle := request.PathValue("gaggle")
		name := request.PathValue("workflow")
		if !validIdentifier(gaggle) {
			writeError(w, http.StatusBadRequest, "invalid_identifier", "gaggle identifier is invalid")
			return
		}
		if !validIdentifier(name) {
			writeError(w, http.StatusBadRequest, "invalid_identifier", "workflow identifier is invalid")
			return
		}
		value, err := reader.Workflow(request.Context(), gaggle, name)
		if err != nil {
			writeInventoryReadError(w, errorLog, "workflow", err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	})
}

func inventoryPageRequest(w http.ResponseWriter, request *http.Request) (readservice.PageRequest, bool) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
		return readservice.PageRequest{}, false
	}
	page := readservice.PageRequest{Cursor: query.Get("cursor")}
	if values := query["cursor"]; len(values) > 1 {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
		return readservice.PageRequest{}, false
	}
	if values := query["limit"]; len(values) > 1 {
		writeError(w, http.StatusBadRequest, "invalid_pagination", "page limit is invalid")
		return readservice.PageRequest{}, false
	}
	if raw, present := query["limit"]; present {
		limit, err := strconv.Atoi(raw[0])
		if err != nil || limit < 1 || limit > readservice.MaxPageSize {
			writeError(w, http.StatusBadRequest, "invalid_pagination", "page limit is invalid")
			return readservice.PageRequest{}, false
		}
		page.Limit = limit
	}
	return page, true
}

func validIdentifier(value string) bool {
	return identifierPattern.MatchString(value)
}

func writeInventoryReadError(w http.ResponseWriter, errorLog *log.Logger, operation string, err error) {
	switch {
	case errors.Is(err, readservice.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "requested resource was not found")
	case errors.Is(err, readservice.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
	case errors.Is(err, readservice.ErrInvalidPage):
		writeError(w, http.StatusBadRequest, "invalid_pagination", "page limit is invalid")
	default:
		errorLog.Printf("%s read failed: %v", operation, err)
		writeError(w, http.StatusInternalServerError, "read_error", "runtime state could not be read")
	}
}
