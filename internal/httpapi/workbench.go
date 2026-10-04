package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workbench"
)

// WorkbenchReadService enforces current human and source authority on every read.
type WorkbenchReadService interface {
	Sources(context.Context, Principal, string) (workbench.SourcePage, error)
	Page(context.Context, Principal, string, string, workbench.BacklogPageRequest) (workbench.BacklogPage, error)
	Get(context.Context, Principal, string, string, workbench.BacklogItemRequest) (workbench.BacklogItem, error)
}

// WithWorkbenchReads installs configured-source metadata and native backlog reads.
func WithWorkbenchReads(service WorkbenchReadService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("workbench read service is required")
		}
		config.workbenchReads = service
		return nil
	}
}
func registerWorkbenchRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	for _, id := range []apicontract.RouteID{apicontract.RouteWorkbenchSources, apicontract.RouteWorkbenchItems, apicontract.RouteWorkbenchItem} {
		router.Handle(id, workbenchHandler(id, config.workbenchReads, errorLog))
	}
}
func workbenchHandler(id apicontract.RouteID, service WorkbenchReadService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		principal, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "workbench_unavailable", "Workbench browsing is unavailable on this server.")
			return
		}
		page, item, err := workbenchReadRequest(r, id)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "Invalid workbench source, item or pagination.")
			return
		}
		gaggle, source := r.PathValue("gaggle"), r.PathValue("source")
		var result any
		switch id {
		case apicontract.RouteWorkbenchSources:
			result, err = service.Sources(r.Context(), principal, gaggle)
		case apicontract.RouteWorkbenchItems:
			result, err = service.Page(r.Context(), principal, gaggle, source, page)
		default:
			result, err = service.Get(r.Context(), principal, gaggle, source, item)
		}
		if err != nil {
			writePlaneError(w, errorLog, "workbench read", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}
func workbenchReadRequest(r *http.Request, id apicontract.RouteID) (workbench.BacklogPageRequest, workbench.BacklogItemRequest, error) {
	page := workbench.BacklogPageRequest{Limit: 50}
	maxItems := workbench.MaxBacklogPageItems
	if id == apicontract.RouteWorkbenchDocuments {
		page.Limit, maxItems = workbench.MaxDocumentPageFiles, workbench.MaxDocumentPageFiles
	}
	item := workbench.BacklogItemRequest{ID: r.PathValue("item")}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return page, item, err
	}
	for key, values := range query {
		allowed := ((id == apicontract.RouteWorkbenchItems || id == apicontract.RouteWorkbenchDocuments) && (key == "cursor" || key == "limit")) || (id == apicontract.RouteWorkbenchItem && key == "expectedSourceId")
		if !allowed || len(values) != 1 || values[0] == "" || !utf8.ValidString(values[0]) || len(values[0]) > workbench.MaxBacklogCursorBytes {
			return page, item, errors.New("invalid workbench query")
		}
	}
	if id == apicontract.RouteWorkbenchSources {
		return page, item, nil
	}
	if r.PathValue("source") == "" || len(r.PathValue("source")) > 64 {
		return page, item, errors.New("invalid source binding")
	}
	if id == apicontract.RouteWorkbenchItem {
		if !workbenchNativeID(item.ID) {
			return page, item, errors.New("invalid native item locator")
		}
		item.ExpectedSourceID = query.Get("expectedSourceId")
		if len(item.ExpectedSourceID) > 512 {
			return page, item, errors.New("invalid expected identity")
		}
		return page, item, nil
	}
	page.Cursor = query.Get("cursor")
	if value := query.Get("limit"); value != "" {
		page.Limit, err = strconv.Atoi(value)
	}
	if err != nil || page.Limit < 1 || page.Limit > maxItems {
		return page, item, errors.New("invalid page limit")
	}
	return page, item, nil
}
func workbenchNativeID(value string) bool {
	n, err := strconv.ParseUint(value, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == value
}
