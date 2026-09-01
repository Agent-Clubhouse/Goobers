package httpapi

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/telemetryclient"
)

// telemetrydefectplane.go is the defect-nomination aggregate read: decision
// 005 R4 as amended by Goobers#4001 (the blocker-1 half of Goobers#3996).
//
// R4's original line put the whole of `telemetry-query` outside the plane,
// because the only shape anyone had for it was "open the rollup database" and
// because the lane that wants it asks for `--aggregate error-signature`, which
// R4 named explicitly. This route keeps both halves of that ruling and moves
// the boundary to the right place:
//
//   - the telemetry DATABASE is still not exposed. No SQL, no table name, no
//     projection name, no file path, and no external-telemetry connector is
//     expressible in a request. The parameter set is closed and enumerated in
//     parseDefectAggregateQuery; anything else is a 400, not an ignored key.
//   - RAW error signatures are still not exposed. What crosses is the
//     NORMALIZED form (telemetryclient.NormalizeErrorSignature): an
//     identifier-shaped classification survives, anything else becomes an
//     opaque digest that still clusters and dedupes. Raw error messages never
//     appear in this projection at all.
//   - the read is gaggle-CONTAINED by the same check the stats, errors and
//     implementation-outcome routes use (containPodTelemetryRead): the daemon
//     resolves the gaggle from the caller's own run, never from the request.
//   - the read is BOUNDED: window, response cardinality and every threshold
//     override have server-side ceilings, and truncation is reported rather
//     than silently applied.
//
// Everything the plane cannot serve fails CLOSED, including a partially
// configured deployment: no service wired is a 503, not an unscoped read.

// TelemetryDefectAggregateService is the daemon-side derivation. The shipped
// implementation lives in cmd/goobers, beside the CLI's own local path and
// sharing its derivation function, so "the daemon answered it" and "the stage
// read it off disk" cannot drift into two different answers — the same
// discipline daemonRunJournalService applies to journalclient.FileCrossRun.
type TelemetryDefectAggregateService interface {
	DefectAggregates(context.Context, TelemetryDefectAggregateRequest) (telemetryclient.DefectAggregateResponse, error)
}

// TelemetryDefectAggregateRequest is one validated, bounded read. It carries
// no path, no query fragment and no projection selector by construction:
// there is no field to put one in.
type TelemetryDefectAggregateRequest struct {
	Gaggle     string
	Workflow   string
	Since      time.Time
	Aggregates []telemetryclient.Aggregate
	Thresholds telemetryclient.Thresholds
}

// WithTelemetryDefectAggregateService enables the defect-aggregate route.
// Without it the route answers 503: a daemon that cannot derive the
// aggregates refuses rather than serving an empty result a nomination lane
// would read as "nothing is wrong".
func WithTelemetryDefectAggregateService(service TelemetryDefectAggregateService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API telemetry defect aggregate service is required")
		}
		config.telemetryDefects = service
		return nil
	}
}

func registerTelemetryDefectAggregateRoute(
	router *Router,
	service TelemetryDefectAggregateService,
	podRunGaggle func(context.Context, string) (string, error),
	errorLog *log.Logger,
) {
	router.Handle(apicontract.RouteTelemetryDefectAggregates, func(w http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "telemetry_unavailable",
				"the telemetry defect-aggregate plane is not available from this server")
			return
		}
		// Containment runs BEFORE the query is parsed, exactly as the other
		// telemetry reads order it: a caller that cannot prove which gaggle
		// it belongs to is refused for that reason, not told which of its
		// parameters was malformed. The gaggle is re-validated as a NAME by
		// parseDefectAggregateQuery immediately after, so passing containment
		// is not a way to smuggle a hostile string through.
		if !containPodTelemetryRead(w, request, podRunGaggle,
			strings.TrimSpace(request.URL.Query().Get("gaggle")), errorLog) {
			return
		}
		query, err := parseDefectAggregateQuery(request.URL.Query(), time.Now().UTC())
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		result, err := service.DefectAggregates(request.Context(), query)
		if err != nil {
			var planeErr *InterventionError
			if errors.As(err, &planeErr) {
				writePlaneError(w, errorLog, "telemetry defect aggregates", err)
				return
			}
			writeTelemetryReadError(w, errorLog, "defect aggregates", err)
			return
		}
		result.Gaggle = query.Gaggle
		result.Workflow = query.Workflow
		result.Since = query.Since
		result.Aggregates = aggregateNames(query.Aggregates)
		writeJSON(w, http.StatusOK, boundDefectAggregateResponse(result))
	})
}

func aggregateNames(aggregates []telemetryclient.Aggregate) []string {
	names := make([]string, 0, len(aggregates))
	for _, aggregate := range aggregates {
		names = append(names, string(aggregate))
	}
	return names
}

// boundDefectAggregateResponse applies the response ceilings as the LAST act
// before serialization, so no service implementation can return more than the
// contract admits — a bound checked in one place cannot be forgotten in
// another. Truncation is reported in the answer; a nomination lane that saw a
// silently shortened list would under-report, which is worse than being told.
func boundDefectAggregateResponse(result telemetryclient.DefectAggregateResponse) telemetryclient.DefectAggregateResponse {
	truncated := false
	if len(result.Findings) > telemetryclient.MaxFindings {
		result.Findings = result.Findings[:telemetryclient.MaxFindings]
		truncated = true
	}
	for i := range result.Findings {
		if len(result.Findings[i].FlaggedRuns) > telemetryclient.MaxFlaggedRuns {
			result.Findings[i].FlaggedRuns = result.Findings[i].FlaggedRuns[:telemetryclient.MaxFlaggedRuns]
			truncated = true
		}
		if result.Findings[i].FlaggedRuns == nil {
			result.Findings[i].FlaggedRuns = []telemetryclient.JournalPointer{}
		}
	}
	if len(result.CausalCredit) > telemetryclient.MaxCausalEstimates {
		result.CausalCredit = result.CausalCredit[:telemetryclient.MaxCausalEstimates]
		truncated = true
	}
	if len(result.PromotionSignals) > telemetryclient.MaxCausalEstimates {
		result.PromotionSignals = result.PromotionSignals[:telemetryclient.MaxCausalEstimates]
		truncated = true
	}
	if len(result.PromotionCandidates) > telemetryclient.MaxCausalEstimates {
		result.PromotionCandidates = result.PromotionCandidates[:telemetryclient.MaxCausalEstimates]
		truncated = true
	}
	if result.Findings == nil {
		result.Findings = []telemetryclient.Finding{}
	}
	if result.PromotionCandidates == nil {
		result.PromotionCandidates = []telemetryclient.PromotionSignal{}
	}
	if truncated {
		result.Truncated = true
		if result.Note == "" {
			result.Note = "the answer was truncated at the plane's cardinality ceiling; narrow the window or the aggregate set"
		}
	}
	return result
}

// defectAggregateQueryParameters is the CLOSED parameter set. It is spelled
// out here, once, and validateQueryValues refuses everything else: a caller
// cannot smuggle a filter the daemon did not intend to offer by adding a key
// the handler happens not to read.
var defectAggregateQueryParameters = []string{
	"gaggle", "workflow", "since", "aggregates",
	"minSamples", "maxFailureRate", "minErrorSignatureCount",
	"minGateEvaluations", "maxGateEscalationRate", "maxFlaggedRuns",
	"minCreditRuns", "minCreditFailureShare",
}

func parseDefectAggregateQuery(values url.Values, now time.Time) (TelemetryDefectAggregateRequest, error) {
	if err := validateQueryValues(values, defectAggregateQueryParameters...); err != nil {
		return TelemetryDefectAggregateRequest{}, err
	}
	gaggle := strings.TrimSpace(values.Get("gaggle"))
	if gaggle == "" {
		return TelemetryDefectAggregateRequest{}, errors.New("gaggle is required; a defect-aggregate read is gaggle-scoped")
	}
	if err := telemetryclient.ValidateScopeName("gaggle", gaggle); err != nil {
		return TelemetryDefectAggregateRequest{}, err
	}
	workflow := strings.TrimSpace(values.Get("workflow"))
	if workflow != "" {
		if err := telemetryclient.ValidateScopeName("workflow", workflow); err != nil {
			return TelemetryDefectAggregateRequest{}, err
		}
	}
	since, err := parseOptionalTime(values.Get("since"), "since")
	if err != nil {
		return TelemetryDefectAggregateRequest{}, err
	}
	if err := telemetryclient.ValidateWindow(since, now); err != nil {
		return TelemetryDefectAggregateRequest{}, err
	}
	aggregates, err := parseDefectAggregates(values)
	if err != nil {
		return TelemetryDefectAggregateRequest{}, err
	}
	thresholds, err := parseDefectAggregateThresholds(values)
	if err != nil {
		return TelemetryDefectAggregateRequest{}, err
	}
	return TelemetryDefectAggregateRequest{
		Gaggle:     gaggle,
		Workflow:   workflow,
		Since:      since,
		Aggregates: aggregates,
		Thresholds: thresholds,
	}, nil
}

// parseDefectAggregates resolves the requested families, defaulting to all
// four admitted ones only when the parameter is ABSENT. An unadmitted name is
// REFUSED rather than dropped: a lane that asked for `learning-episode` and
// silently got three families back would file fewer nominations and never
// learn why. An empty value is refused for the same reason — a caller that
// computed an empty list asked for nothing, not for everything.
func parseDefectAggregates(values url.Values) ([]telemetryclient.Aggregate, error) {
	if !values.Has("aggregates") {
		return telemetryclient.AdmittedAggregates(), nil
	}
	trimmed := strings.TrimSpace(values.Get("aggregates"))
	if trimmed == "" {
		return nil, errors.New("aggregates is present but empty; omit it to request every admitted aggregate")
	}
	var aggregates []telemetryclient.Aggregate
	for _, name := range strings.Split(trimmed, ",") {
		aggregate, err := telemetryclient.ParseAggregate(name)
		if err != nil {
			return nil, err
		}
		if !slicesContainsAggregate(aggregates, aggregate) {
			aggregates = append(aggregates, aggregate)
		}
	}
	return aggregates, nil
}

func slicesContainsAggregate(set []telemetryclient.Aggregate, want telemetryclient.Aggregate) bool {
	for _, aggregate := range set {
		if aggregate == want {
			return true
		}
	}
	return false
}

func parseDefectAggregateThresholds(values url.Values) (telemetryclient.Thresholds, error) {
	var thresholds telemetryclient.Thresholds
	count := func(name string, max int) (int, error) {
		raw := values.Get(name)
		if raw == "" {
			return 0, nil
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > max {
			return 0, errFormat(name, "an integer between 1 and", max)
		}
		return parsed, nil
	}
	rate := func(name string) (float64, error) {
		raw := values.Get(name)
		if raw == "" {
			return 0, nil
		}
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 1 {
			return 0, errors.New(name + " must be a number between 0 and 1")
		}
		return parsed, nil
	}
	var err error
	if thresholds.MinSamples, err = count("minSamples", telemetryclient.MaxThresholdCount); err != nil {
		return telemetryclient.Thresholds{}, err
	}
	if thresholds.MaxFailureRate, err = rate("maxFailureRate"); err != nil {
		return telemetryclient.Thresholds{}, err
	}
	if thresholds.MinErrorSignatureCount, err = count("minErrorSignatureCount", telemetryclient.MaxThresholdCount); err != nil {
		return telemetryclient.Thresholds{}, err
	}
	if thresholds.MinGateEvaluations, err = count("minGateEvaluations", telemetryclient.MaxThresholdCount); err != nil {
		return telemetryclient.Thresholds{}, err
	}
	if thresholds.MaxGateEscalationRate, err = rate("maxGateEscalationRate"); err != nil {
		return telemetryclient.Thresholds{}, err
	}
	if thresholds.MaxFlaggedRuns, err = count("maxFlaggedRuns", telemetryclient.MaxFlaggedRuns); err != nil {
		return telemetryclient.Thresholds{}, err
	}
	if thresholds.MinCreditRuns, err = count("minCreditRuns", telemetryclient.MaxThresholdCount); err != nil {
		return telemetryclient.Thresholds{}, err
	}
	if thresholds.MinCreditFailureShare, err = rate("minCreditFailureShare"); err != nil {
		return telemetryclient.Thresholds{}, err
	}
	if err := telemetryclient.ValidateThresholds(thresholds); err != nil {
		return telemetryclient.Thresholds{}, err
	}
	return thresholds, nil
}

func errFormat(name, shape string, max int) error {
	return errors.New(name + " must be " + shape + " " + strconv.Itoa(max))
}
