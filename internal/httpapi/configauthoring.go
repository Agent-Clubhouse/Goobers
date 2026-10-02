package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/configauthoring"
)

// ConfigAuthoringReader serves browser-safe configuration source reads.
type ConfigAuthoringReader interface {
	Sources(context.Context) (apicontract.ConfigSourcePage, error)
	Documents(context.Context, string) (apicontract.ConfigDocumentPage, error)
	Document(context.Context, string, string) (apicontract.ConfigDocument, error)
}

func registerConfigAuthoringReadRoutes(router *Router, reader ConfigAuthoringReader, errorLog *log.Logger) {
	router.handleAuthoring(apicontract.RouteConfigSources, func(w http.ResponseWriter, request *http.Request) {
		if !requireConfigViewer(w, request) {
			return
		}
		page, err := reader.Sources(request.Context())
		if err != nil {
			writeConfigReadError(w, errorLog, "list configuration sources", err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	router.handleAuthoring(apicontract.RouteConfigSourceDocuments, func(w http.ResponseWriter, request *http.Request) {
		if !requireConfigViewer(w, request) {
			return
		}
		page, err := reader.Documents(request.Context(), request.PathValue("source"))
		if err != nil {
			writeConfigReadError(w, errorLog, "list configuration documents", err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	router.handleAuthoring(apicontract.RouteConfigSourceDocument, func(w http.ResponseWriter, request *http.Request) {
		if !requireConfigViewer(w, request) {
			return
		}
		document, err := reader.Document(request.Context(), request.PathValue("source"), request.URL.Query().Get("path"))
		if err != nil {
			writeConfigReadError(w, errorLog, "read configuration document", err)
			return
		}
		writeJSON(w, http.StatusOK, document)
	})
}

func requireConfigViewer(w http.ResponseWriter, request *http.Request) bool {
	principal, authenticated := PrincipalFromRequest(request)
	if !authenticated {
		writeConfigAuthoringError(w, http.StatusUnauthorized, apicontract.CodeConfigAuthorizationFailed, "request is not authenticated")
		return false
	}
	if !principal.HasRole(RoleView) {
		writeConfigAuthoringError(w, http.StatusForbidden, apicontract.CodeConfigAuthorizationFailed, "request is not authorized")
		return false
	}
	return true
}

func writeConfigReadError(w http.ResponseWriter, errorLog *log.Logger, operation string, err error) {
	switch {
	case errors.Is(err, configauthoring.ErrSourceNotFound):
		writeConfigAuthoringError(w, http.StatusNotFound, apicontract.CodeConfigSourceNotFound, "configuration source was not found")
	case errors.Is(err, configauthoring.ErrDocumentNotFound):
		writeConfigAuthoringError(w, http.StatusNotFound, apicontract.CodeConfigDocumentNotFound, "configuration document was not found")
	default:
		errorLog.Printf("%s failed: %v", operation, err)
		writeError(w, http.StatusInternalServerError, "config_read_failed", "configuration could not be read")
	}
}

func writeConfigAuthoringError(w http.ResponseWriter, status int, code apicontract.ConfigAuthoringErrorCode, message string) {
	writeJSON(w, status, apicontract.ConfigAuthoringErrorEnvelope{
		Error: apicontract.ConfigAuthoringError{Code: code, Message: message},
	})
}
