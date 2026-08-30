package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

const (
	defaultTelemetryErrorSignaturesLimit = 20
	maxTelemetryErrorsPageSize           = 200
)

func registerTelemetryRoutes(router *Router, reader readservice.TelemetryReader, podRunGaggle func(context.Context, string) (string, error), errorLog *log.Logger) {
	router.Handle(apicontract.RouteTelemetryStats, func(w http.ResponseWriter, request *http.Request) {
		query, err := parseTelemetryStatsQuery(request.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if !containPodTelemetryRead(w, request, podRunGaggle, query.Gaggle, errorLog) {
			return
		}
		result, err := reader.TelemetryStats(request.Context(), query)
		if err != nil {
			writeTelemetryReadError(w, errorLog, "stats", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	router.Handle(apicontract.RouteTelemetryErrorSignatures, func(w http.ResponseWriter, request *http.Request) {
		query, err := parseTelemetryErrorSignaturesQuery(request.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		result, err := reader.TelemetryErrorSignatures(request.Context(), query)
		if err != nil {
			writeTelemetryReadError(w, errorLog, "error signatures", err)
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
		if !containPodTelemetryRead(w, request, podRunGaggle, query.Gaggle, errorLog) {
			return
		}
		result, err := reader.TelemetryErrors(request.Context(), query)
		if err != nil {
			writeTelemetryReadError(w, errorLog, "errors", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	router.Handle(apicontract.RouteTelemetryImplementationOutcomes, func(w http.ResponseWriter, request *http.Request) {
		query, err := parseTelemetryImplementationOutcomesQuery(request.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if !containPodTelemetryRead(w, request, podRunGaggle, query.Gaggle, errorLog) {
			return
		}
		result, err := reader.TelemetryImplementationOutcomes(request.Context(), query)
		if err != nil {
			writeTelemetryReadError(w, errorLog, "implementation outcomes", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

// containPodTelemetryRead applies decision 005 R4's containment to the
// telemetry read routes a pod principal may reach and reports whether the
// request may proceed. It writes the refusal itself when it returns false.
//
// Human principals are untouched: they went through the role ladder in
// RequireRoles and keep exactly the unscoped access they have always had, so
// no existing dashboard or CLI query changes shape.
//
// For a pod principal every branch fails CLOSED, because each one means the
// daemon cannot prove the read stays inside the pod's own gaggle:
//
//   - no resolver wired: the deployment never opted into the pod telemetry
//     read (WithPodRunGaggle), so there is nothing to check against;
//   - no gaggle in the query: an unscoped read is a cross-gaggle read;
//   - the pod's run has no resolvable gaggle, or resolution fails: unknown
//     scope is not "any scope";
//   - resolved gaggle differs from the requested one: the pod is asking about
//     someone else's backlog.
//
// The refusal body names the mismatch class but never the resolved gaggle: a
// pod that guessed wrong learns that it guessed wrong, not what the right
// answer was.
func containPodTelemetryRead(
	w http.ResponseWriter,
	request *http.Request,
	podRunGaggle func(context.Context, string) (string, error),
	requestedGaggle string,
	errorLog *log.Logger,
) bool {
	principal, authenticated := PrincipalFromRequest(request)
	if !authenticated || !IsPodPrincipal(principal) {
		return true
	}
	if podRunGaggle == nil {
		writeError(w, http.StatusForbidden, "telemetry_scope_unavailable",
			"this daemon cannot scope a pod telemetry read to its gaggle; the read is refused")
		return false
	}
	if strings.TrimSpace(requestedGaggle) == "" {
		writeError(w, http.StatusForbidden, "gaggle_required",
			"pod principals must scope a telemetry read to their own gaggle")
		return false
	}
	runID, ok := strings.CutPrefix(principal.Subject, "run:")
	if !ok || strings.TrimSpace(runID) == "" {
		writeError(w, http.StatusForbidden, "gaggle_mismatch",
			"pod principal may only read its own gaggle's telemetry")
		return false
	}
	gaggle, err := podRunGaggle(request.Context(), runID)
	if err != nil {
		errorLog.Printf("telemetry read: resolve gaggle for run %q: %v", runID, err)
		writeError(w, http.StatusForbidden, "gaggle_mismatch",
			"pod principal may only read its own gaggle's telemetry")
		return false
	}
	if strings.TrimSpace(gaggle) == "" || gaggle != requestedGaggle {
		writeError(w, http.StatusForbidden, "gaggle_mismatch",
			"pod principal may only read its own gaggle's telemetry")
		return false
	}
	return true
}

func parseTelemetryImplementationOutcomesQuery(values url.Values) (readservice.TelemetryImplementationOutcomesRequest, error) {
	if err := validateQueryValues(values, "gaggle", "since"); err != nil {
		return readservice.TelemetryImplementationOutcomesRequest{}, err
	}
	since, err := parseOptionalTime(values.Get("since"), "since")
	if err != nil {
		return readservice.TelemetryImplementationOutcomesRequest{}, err
	}
	return readservice.TelemetryImplementationOutcomesRequest{
		Gaggle: values.Get("gaggle"),
		Since:  since,
	}, nil
}

func parseTelemetryStatsQuery(values url.Values) (readservice.TelemetryStatsRequest, error) {
	if err := validateQueryValues(values, "workflow", "gaggle", "branch", "model", "harnessVersion", "groupBy", "since", "until", "trendSince", "trendUntil", "trendBuckets", "trendPreviousSince", "trendPreviousUntil"); err != nil {
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
	trendSince, err := parseOptionalTime(values.Get("trendSince"), "trendSince")
	if err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	trendUntil, err := parseOptionalTime(values.Get("trendUntil"), "trendUntil")
	if err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	trendPreviousSince, err := parseOptionalTime(values.Get("trendPreviousSince"), "trendPreviousSince")
	if err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	trendPreviousUntil, err := parseOptionalTime(values.Get("trendPreviousUntil"), "trendPreviousUntil")
	if err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	if err := validateTelemetryTrendWindow(trendSince, trendUntil, "trend"); err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	if err := validateTelemetryTrendWindow(trendPreviousSince, trendPreviousUntil, "trend previous"); err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	trendBuckets := 0
	if values.Has("trendBuckets") {
		trendBuckets, err = strconv.Atoi(values.Get("trendBuckets"))
		if err != nil || trendBuckets < 1 || trendBuckets > 100 || trendSince.IsZero() || trendUntil.IsZero() || !trendSince.Before(trendUntil) {
			return readservice.TelemetryStatsRequest{}, errors.New("trendBuckets requires a valid trendSince/trendUntil range and must be between 1 and 100")
		}
	}
	var branch *int
	if values.Has("branch") {
		value, parseErr := strconv.Atoi(values.Get("branch"))
		if parseErr != nil || value < 0 {
			return readservice.TelemetryStatsRequest{}, errors.New("branch must be a non-negative integer")
		}
		branch = &value
	}
	groupByBranch, groupByModel, groupByHarnessVersion, err := parseTelemetryGroupBy(values.Get("groupBy"))
	if err != nil {
		return readservice.TelemetryStatsRequest{}, err
	}
	return readservice.TelemetryStatsRequest{
		Workflow:              values.Get("workflow"),
		Gaggle:                values.Get("gaggle"),
		Branch:                branch,
		Model:                 values.Get("model"),
		HarnessVersion:        values.Get("harnessVersion"),
		GroupByBranch:         groupByBranch,
		GroupByModel:          groupByModel,
		GroupByHarnessVersion: groupByHarnessVersion,
		Since:                 since,
		Until:                 until,
		TrendSince:            trendSince,
		TrendUntil:            trendUntil,
		TrendBuckets:          trendBuckets,
		TrendPreviousSince:    trendPreviousSince,
		TrendPreviousUntil:    trendPreviousUntil,
	}, nil
}

func validateTelemetryTrendWindow(since, until time.Time, name string) error {
	if since.IsZero() && until.IsZero() {
		return nil
	}
	if since.IsZero() || until.IsZero() || !since.Before(until) {
		return fmt.Errorf("%sSince and %sUntil must form an increasing range", name, name)
	}
	return nil
}

func parseTelemetryGroupBy(value string) (branch, model, harnessVersion bool, err error) {
	if value == "" {
		return false, false, false, nil
	}
	for _, dimension := range strings.Split(value, ",") {
		switch dimension {
		case "branch":
			branch = true
		case "model":
			model = true
		case "harness-version", "harnessVersion":
			harnessVersion = true
		default:
			return false, false, false, fmt.Errorf("groupBy contains unknown dimension %q", dimension)
		}
	}
	return branch, model, harnessVersion, nil
}

func parseTelemetryErrorSignaturesQuery(values url.Values) (readservice.TelemetryErrorSignaturesRequest, error) {
	if err := validateQueryValues(values, "workflow", "gaggle", "stage", "since", "until", "limit"); err != nil {
		return readservice.TelemetryErrorSignaturesRequest{}, err
	}
	since, until, err := parseTelemetryWindow(values)
	if err != nil {
		return readservice.TelemetryErrorSignaturesRequest{}, err
	}
	limit := defaultTelemetryErrorSignaturesLimit
	if value := values.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > maxTelemetryErrorsPageSize {
			return readservice.TelemetryErrorSignaturesRequest{}, fmt.Errorf("limit must be an integer between 1 and %d", maxTelemetryErrorsPageSize)
		}
	}
	return readservice.TelemetryErrorSignaturesRequest{
		Workflow: values.Get("workflow"),
		Gaggle:   values.Get("gaggle"),
		Stage:    values.Get("stage"),
		Since:    since,
		Until:    until,
		Limit:    limit,
	}, nil
}

func parseTelemetryErrorsQuery(values url.Values) (readservice.TelemetryErrorsRequest, error) {
	if err := validateQueryValues(values, "workflow", "gaggle", "stage", "code", "class", "since", "until", "limit", "cursor"); err != nil {
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
		Workflow:         values.Get("workflow"),
		Gaggle:           values.Get("gaggle"),
		Stage:            values.Get("stage"),
		Code:             values.Get("code"),
		ErrorClass:       values.Get("class"),
		FilterCode:       values.Has("code"),
		FilterErrorClass: values.Has("class"),
		Since:            since,
		Until:            until,
		Limit:            limit,
		Cursor:           values.Get("cursor"),
	}, nil
}

func parseTelemetryWindow(values url.Values) (time.Time, time.Time, error) {
	since, err := parseOptionalTime(values.Get("since"), "since")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	until, err := parseOptionalTime(values.Get("until"), "until")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return time.Time{}, time.Time{}, errors.New("since must not be after until")
	}
	return since, until, nil
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
	if clientCancelled(w, err) {
		return
	}
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
