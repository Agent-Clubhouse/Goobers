package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/gagglebundle"
)

const maxGaggleBundleBody = 16 << 20

// GaggleBundleService exports sanitized definitions and atomically creates a
// destination gaggle from a fully validated bundle.
type GaggleBundleService interface {
	ExportGaggle(ctx context.Context, name string) (apiv1.GaggleBundle, error)
	ImportGaggle(ctx context.Context, input apiv1.GaggleBundleImportRequest) (apiv1.GaggleBundleImportResult, error)
}

func registerGaggleBundleRoutes(router *Router, service GaggleBundleService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteGaggleBundleExport, gaggleBundleExportHandler(service, errorLog))
	router.Handle(apicontract.RouteGaggleBundleImport, gaggleBundleImportHandler(service, errorLog))
}

func gaggleBundleExportHandler(service GaggleBundleService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "gaggle_bundles_unavailable", "gaggle bundle export is not available from this server")
			return
		}
		bundle, err := service.ExportGaggle(request.Context(), request.PathValue("gaggle"))
		if err != nil {
			writeGaggleBundleError(w, err, errorLog)
			return
		}
		writeJSON(w, http.StatusOK, bundle)
	}
}

func gaggleBundleImportHandler(service GaggleBundleService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "gaggle_bundles_unavailable", "gaggle bundle import is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		input, err := decodeGaggleBundleImportRequest(request)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		result, err := service.ImportGaggle(request.Context(), input)
		if err != nil {
			writeGaggleBundleError(w, err, errorLog)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func decodeGaggleBundleImportRequest(request *http.Request) (apiv1.GaggleBundleImportRequest, error) {
	defer func() { _ = request.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxGaggleBundleBody))
	decoder.DisallowUnknownFields()
	var input apiv1.GaggleBundleImportRequest
	if err := decoder.Decode(&input); err != nil {
		if errors.Is(err, io.EOF) {
			return input, errors.New("JSON request body is required")
		}
		return input, fmt.Errorf("invalid JSON request body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return input, errors.New("request body must contain one JSON object")
		}
		return input, fmt.Errorf("invalid JSON request body: %w", err)
	}
	return input, nil
}

func writeGaggleBundleError(w http.ResponseWriter, err error, errorLog *log.Logger) {
	var interventionErr *InterventionError
	if errors.As(err, &interventionErr) {
		status := interventionErr.Status
		if status < 400 || status > 599 {
			status = http.StatusInternalServerError
		}
		writeError(w, status, interventionErr.Code, interventionErr.Message)
		return
	}
	switch {
	case errors.Is(err, gagglebundle.ErrGaggleNotFound):
		writeError(w, http.StatusNotFound, "gaggle_not_found", err.Error())
	case errors.Is(err, gagglebundle.ErrNameConflict):
		writeError(w, http.StatusConflict, "gaggle_name_conflict", err.Error())
	case errors.Is(err, gagglebundle.ErrRepositoryAuthorization):
		writeError(w, http.StatusPreconditionFailed, "repository_authorization_required", err.Error())
	case errors.Is(err, gagglebundle.ErrInvalidBundle):
		writeError(w, http.StatusUnprocessableEntity, "invalid_gaggle_bundle", err.Error())
	default:
		errorLog.Printf("gaggle bundle operation failed: %v", err)
		writeError(w, http.StatusInternalServerError, "gaggle_bundle_failed", "gaggle bundle operation failed")
	}
}
