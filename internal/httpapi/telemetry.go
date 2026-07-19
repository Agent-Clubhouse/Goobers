package httpapi

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

const maxTelemetryErrorsPageSize = 200

func registerTelemetryRoutes(router *Router, reader readservice.TelemetryReader, errorLog *log.Logger) {
	router.Handle(apicontract.RouteTelemetryStats, func(w http.ResponseWriter, request *http.Request) {
		query, err := parseTelemetryStatsQuery(request.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		result, err := reader.TelemetryStats(request.Context(), query)
		if err != nil {
			writeTelemetryReadError(w, errorLog, "stats", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	router.Handle(apicontract.RouteTelemetryErrors, func(w http.ResponseWriter, request *http.Request) {
		query, err := parseTelemetryErrorsQuery(request.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		result, err := reader.TelemetryErrors(request.Context(), query)
		if err != nil {
			writeTelemetryReadError(w, errorLog, "errors", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

func parseTelemetryStatsQuery(values url.Values) (readservice.TelemetryStatsRequest, error) {
	if err := validateQueryValues(values, "workflow", "gaggle", "since", "until"); err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	since, err := parseOptionalTime(values.Get("since"), "since")
	if err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	until, err := parseOptionalTime(values.Get("until"), "until")
	if err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return readservice.TelemetryStatsRequest{}, errors.New("since must not be after until")
	}
	return readservice.TelemetryStatsRequest{
		Workflow: values.Get("workflow"),
		Gaggle:   values.Get("gaggle"),
		Since:    since,
		Until:    until,
	}, nil
}

func parseTelemetryErrorsQuery(values url.Values) (readservice.TelemetryErrorsRequest, error) {
	if err := validateQueryValues(values, "workflow", "gaggle", "class", "since", "until", "limit", "cursor"); err != nil {
		return readservice.TelemetryErrorsRequest{}, err
	}
	since, err := parseOptionalTime(values.Get("since"), "since")
	if err != nil {
		return readservice.TelemetryErrorsRequest{}, err
	}
	until, err := parseOptionalTime(values.Get("until"), "until")
	if err != nil {
		return readservice.TelemetryErrorsRequest{}, err
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return readservice.TelemetryErrorsRequest{}, errors.New("since must not be after until")
	}
	limit := 50
	if value := values.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > maxTelemetryErrorsPageSize {
			return readservice.TelemetryErrorsRequest{}, fmt.Errorf("limit must be an integer between 1 and %d", maxTelemetryErrorsPageSize)
		}
	}
	return readservice.TelemetryErrorsRequest{
		Workflow:   values.Get("workflow"),
		Gaggle:     values.Get("gaggle"),
		ErrorClass: values.Get("class"),
		Since:      since,
		Until:      until,
		Limit:      limit,
		Cursor:     values.Get("cursor"),
	}, nil
}

func validateQueryValues(values url.Values, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name, entries := range values {
		if _, ok := allowedSet[name]; !ok {
			return fmt.Errorf("unknown query parameter %q", name)
		}
		if len(entries) != 1 {
			return fmt.Errorf("query parameter %q must be specified once", name)
		}
	}
	return nil
}

func parseOptionalTime(value, name string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp", name)
	}
	return parsed, nil
}

func writeTelemetryReadError(w http.ResponseWriter, errorLog *log.Logger, projection string, err error) {
	switch {
	case errors.Is(err, readservice.ErrInvalidTelemetryRequest):
		writeError(w, http.StatusBadRequest, "invalid_query", "telemetry query is invalid")
	case errors.Is(err, readservice.ErrTelemetryUnavailable):
		writeError(w, http.StatusServiceUnavailable, "telemetry_unavailable", "telemetry is not enabled")
	default:
		errorLog.Printf("telemetry %s read failed: %v", projection, err)
		writeError(w, http.StatusInternalServerError, "read_error", "telemetry could not be read")
	}
}
