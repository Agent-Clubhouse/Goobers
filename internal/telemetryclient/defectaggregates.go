package telemetryclient

// defectaggregates.go is the defect-nomination aggregate read — the narrowed
// option (2) ruled on Goobers#3996 blocker 1 and built as Goobers#4001.
//
// What crosses the boundary is a FIXED, closed set of derived aggregates:
// stage-failure-rate, gate-noise, credit-assignment, and a NORMALIZED,
// REDACTED error-signature aggregate. What does not cross it, and is not
// expressible in a request at all: the telemetry database, any raw rollup
// row, any raw error message, any external-telemetry connector, any SQL, any
// path, any projection name. The client names a gaggle, a bounded window,
// which of the four families it wants, and bounded numeric thresholds; the
// DAEMON queries, filters, aggregates and redacts.
//
// The wire shapes here are restated rather than imported from the daemon's
// packages, exactly as ImplementationOutcome above is: a stage-side client
// depends on the CONTRACT, not on internal/readservice, internal/readmodel or
// internal/telemetry/rollup. The server's tests pin the two together.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// Aggregate names one admitted detection family.
type Aggregate string

// The four admitted aggregate families, and only these four. `all`,
// `ci-check-failure`, `workflow-untriggered`, `stage-unreached` and
// `learning-episode` are deliberately absent: the ruling enumerated four, so
// four is what this admits, and asking for a fifth is refused rather than
// silently narrowed.
const (
	AggregateStageFailureRate Aggregate = "stage-failure-rate"
	AggregateErrorSignature   Aggregate = "error-signature"
	AggregateGateNoise        Aggregate = "gate-noise"
	AggregateCreditAssignment Aggregate = "credit-assignment"
)

// AdmittedAggregates returns the closed admitted set, in wire order.
func AdmittedAggregates() []Aggregate {
	return []Aggregate{
		AggregateStageFailureRate,
		AggregateErrorSignature,
		AggregateGateNoise,
		AggregateCreditAssignment,
	}
}

// ParseAggregate resolves one wire name, refusing anything outside the
// admitted set — including the names the local CLI accepts. A caller asking
// for `all` or `learning-episode` gets a refusal naming what IS admitted,
// never a quietly narrowed answer.
func ParseAggregate(raw string) (Aggregate, error) {
	candidate := Aggregate(strings.TrimSpace(raw))
	for _, admitted := range AdmittedAggregates() {
		if candidate == admitted {
			return admitted, nil
		}
	}
	return "", fmt.Errorf("aggregate %q is not served by the telemetry aggregate plane (admitted: %s)",
		raw, JoinAggregates(AdmittedAggregates()))
}

// JoinAggregates renders a set for an error message or a query parameter.
func JoinAggregates(aggregates []Aggregate) string {
	names := make([]string, 0, len(aggregates))
	for _, aggregate := range aggregates {
		names = append(names, string(aggregate))
	}
	return strings.Join(names, ",")
}

// Bounds on one defect-aggregate request and its answer. Every one of them is
// enforced on the SERVER as well as here: a client-side bound is a courtesy,
// not a control.
const (
	// MaxWindow bounds the lookback a single read may ask for. The lanes that
	// consume this plane use 24h and 168h; a month is generous headroom, and
	// an unbounded walk of the whole rollup is exactly the shape a bounded
	// read plane exists to refuse.
	MaxWindow = 720 * time.Hour
	// MaxFlaggedRuns bounds the example runs one finding may carry.
	MaxFlaggedRuns = 50
	// MaxFindings bounds how many findings one answer may carry.
	MaxFindings = 500
	// MaxCausalEstimates bounds the causal-credit and promotion-signal lists.
	MaxCausalEstimates = 500
	// MaxThresholdCount bounds every count-shaped threshold override, so a
	// caller cannot turn a threshold into an unbounded scan request.
	MaxThresholdCount = 10_000
	// ClockSkewTolerance is how far into the future a caller's `since` may be
	// before the plane refuses it. Pods and daemons are separate machines;
	// a small skew is normal, an hour of it is a malformed window.
	ClockSkewTolerance = 5 * time.Minute
)

// Thresholds are the bounded numeric detection knobs a defect-aggregate read
// may override. Deliberately a SUBSET of the CLI's own threshold set: the
// families this plane does not serve have no knobs here, so a request cannot
// even name one.
//
// A zero field means "use the daemon's default" — the same treatment the
// local path gives an unset --threshold.
type Thresholds struct {
	MinSamples             int     `json:"minSamples,omitempty"`
	MaxFailureRate         float64 `json:"maxFailureRate,omitempty"`
	MinErrorSignatureCount int     `json:"minErrorSignatureCount,omitempty"`
	MinGateEvaluations     int     `json:"minGateEvaluations,omitempty"`
	MaxGateEscalationRate  float64 `json:"maxGateEscalationRate,omitempty"`
	MaxFlaggedRuns         int     `json:"maxFlaggedRuns,omitempty"`
	MinCreditRuns          int     `json:"minCreditRuns,omitempty"`
	MinCreditFailureShare  float64 `json:"minCreditFailureShare,omitempty"`
}

// DefectAggregateRequest is one bounded read.
type DefectAggregateRequest struct {
	// Gaggle is the containment key. The client always sends its OWN gaggle;
	// the daemon checks it against the gaggle it resolves from the bearer's
	// run, never against what the caller claims.
	Gaggle string
	// Workflow optionally narrows every family to one workflow in that
	// gaggle. It is a filter, not a path.
	Workflow string
	// Since is the inclusive lower bound. Required: an unbounded read is
	// refused on both sides.
	Since time.Time
	// Aggregates is the requested subset of the admitted four. Empty means
	// all four.
	Aggregates []Aggregate
	// Thresholds overrides the daemon's detection defaults, within bounds.
	Thresholds Thresholds
}

// JournalPointer names one example run. Run ids inside the caller's own
// gaggle are already reachable through the run-scoped read routes; nothing
// about the run's content travels with the pointer.
type JournalPointer struct {
	RunID string `json:"runId"`
	Seq   uint64 `json:"seq,omitempty"`
}

// NominationGuardrails restates the machine-readable checks a credit-assignment
// finding carries into the filer.
type NominationGuardrails struct {
	DedupeKey                  string `json:"dedupe_key"`
	RequiresUpstreamCauseCheck bool   `json:"requires_upstream_cause_check"`
	RequiresHumanReview        bool   `json:"requires_human_review"`
	GoverningTargetTreatment   string `json:"governing_target_treatment"`
}

// Finding is one derived, threshold-crossing candidate. Tag for tag the
// candidate-findings-v1 schema's finding shape, so a client can place it into
// the artifact it already emits without a second translation.
type Finding struct {
	Kind                 string                `json:"kind"`
	Subject              string                `json:"subject"`
	Metrics              map[string]float64    `json:"metrics"`
	Threshold            float64               `json:"threshold"`
	FlaggedRuns          []JournalPointer      `json:"flagged_runs"`
	Signature            string                `json:"signature,omitempty"`
	Classification       string                `json:"classification,omitempty"`
	RecommendedAction    string                `json:"recommendedAction,omitempty"`
	NominationGuardrails *NominationGuardrails `json:"nomination_guardrails,omitempty"`
}

// CausalNodeCredit is one node's causal estimate, or its explicit
// cannot-identify verdict.
type CausalNodeCredit struct {
	Node              string  `json:"node"`
	Effect            float64 `json:"effect"`
	Lower             float64 `json:"lower"`
	Upper             float64 `json:"upper"`
	Identification    string  `json:"identification"`
	Caveat            string  `json:"caveat"`
	TreatedBefore     int     `json:"treatedBefore"`
	TreatedAfter      int     `json:"treatedAfter"`
	ControlBefore     int     `json:"controlBefore"`
	ControlAfter      int     `json:"controlAfter"`
	IntervalAvailable bool    `json:"intervalAvailable"`
	PromotionEligible bool    `json:"promotionEligible"`
	PromotionSource   string  `json:"promotionSource"`
}

// PromotionSignal is bounded promotion evidence for one node.
type PromotionSignal struct {
	Node              string  `json:"node"`
	Value             float64 `json:"value"`
	Lower             float64 `json:"lower,omitempty"`
	Upper             float64 `json:"upper,omitempty"`
	Source            string  `json:"source"`
	Caveat            string  `json:"caveat"`
	PromotionEligible bool    `json:"promotionEligible"`
}

// DefectAggregateResponse is the plane's answer.
//
// Truncated is loud on purpose: a bounded answer that silently dropped
// findings would make a nomination lane quietly under-report, which is the
// same silent-wrong-result class the dispatch refusals exist to prevent.
type DefectAggregateResponse struct {
	Gaggle              string             `json:"gaggle"`
	Workflow            string             `json:"workflow,omitempty"`
	Since               time.Time          `json:"since"`
	Aggregates          []string           `json:"aggregates"`
	Findings            []Finding          `json:"findings"`
	CausalCredit        []CausalNodeCredit `json:"causalCredit,omitempty"`
	PromotionSignals    []PromotionSignal  `json:"promotionSignals,omitempty"`
	PromotionCandidates []PromotionSignal  `json:"promotionCandidates"`
	NoWork              bool               `json:"noWork,omitempty"`
	Truncated           bool               `json:"truncated,omitempty"`
	Note                string             `json:"note,omitempty"`
}

// safeSignatureToken is what an error code must already look like to survive
// normalization unchanged: a short, identifier-shaped, stable token — which is
// what the rollup's own `code` column holds for every error the daemon
// classifies. Anything else (a path, a URL, a quoted message, a token, an
// address, a stack frame) is replaced by an opaque digest.
var safeSignatureToken = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:+-]{0,63}$`)

// ErrorSignaturePrefix labels the stable digest a normalized error signature
// carries so a consumer can dedupe on it without ever seeing the raw code.
const ErrorSignaturePrefix = "error-signature:sha256:"

// RedactedSignatureSubject is the subject a code that is not already a safe
// token is replaced by. It carries the same digest the signature does,
// truncated, so two occurrences of one hostile code still cluster while
// neither reveals the code.
const RedactedSignatureSubject = "redacted-error-signature"

// NormalizeErrorSignature is decision 005 R4's line, drawn as a function:
// raw error signatures stay off the plane, DERIVED normalized ones are
// admitted (Goobers#4001).
//
// A code that is already an identifier-shaped token is its own normal form —
// `stage_timeout` and `provider_rate_limited` are classifications, not
// content, and preserving them is what keeps the plane's answer as useful to
// a nominator as the local one. Anything else is replaced by
// `redacted-error-signature:<12 hex>` — enough to cluster and dedupe, not
// enough to reconstruct. The full-strength digest is returned alongside as
// the finding's stable signature either way.
//
// It is deliberately total: every input has a normal form, so no error path
// can leak a raw code by falling through.
func NormalizeErrorSignature(code string) (subject, signature string) {
	trimmed := strings.TrimSpace(code)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(trimmed)))
	signature = ErrorSignaturePrefix + digest
	if trimmed != "" && safeSignatureToken.MatchString(trimmed) {
		return trimmed, signature
	}
	return RedactedSignatureSubject + ":" + digest[:12], signature
}

// ValidateWindow bounds a read's lookback against now, on the client side
// too, so a request the plane would refuse is not sent at all.
func ValidateWindow(since, now time.Time) error {
	if since.IsZero() {
		return fmt.Errorf("telemetryclient: a defect-aggregate read requires a since bound; an unbounded rollup walk is refused")
	}
	if since.After(now.Add(ClockSkewTolerance)) {
		return fmt.Errorf("telemetryclient: since is in the future")
	}
	if now.Sub(since) > MaxWindow {
		return fmt.Errorf("telemetryclient: window %s exceeds the %s ceiling this plane serves", now.Sub(since).Round(time.Second), MaxWindow)
	}
	return nil
}

// ValidateThresholds bounds every override. Rates are fractions in [0, 1];
// counts are positive and capped, so no threshold can be used to widen the
// read past the response bounds.
func ValidateThresholds(thresholds Thresholds) error {
	counts := []struct {
		name  string
		value int
		max   int
	}{
		{"minSamples", thresholds.MinSamples, MaxThresholdCount},
		{"minErrorSignatureCount", thresholds.MinErrorSignatureCount, MaxThresholdCount},
		{"minGateEvaluations", thresholds.MinGateEvaluations, MaxThresholdCount},
		{"maxFlaggedRuns", thresholds.MaxFlaggedRuns, MaxFlaggedRuns},
		{"minCreditRuns", thresholds.MinCreditRuns, MaxThresholdCount},
	}
	for _, count := range counts {
		if count.value < 0 || count.value > count.max {
			return fmt.Errorf("telemetryclient: %s must be between 0 and %d", count.name, count.max)
		}
	}
	rates := []struct {
		name  string
		value float64
	}{
		{"maxFailureRate", thresholds.MaxFailureRate},
		{"maxGateEscalationRate", thresholds.MaxGateEscalationRate},
		{"minCreditFailureShare", thresholds.MinCreditFailureShare},
	}
	for _, rate := range rates {
		if rate.value < 0 || rate.value > 1 {
			return fmt.Errorf("telemetryclient: %s must be a fraction between 0 and 1", rate.name)
		}
	}
	return nil
}

// DefectAggregateQuery renders a validated request as the plane's closed
// query-parameter set. Every name here is on the server's allowlist; a name
// that is not is refused there rather than ignored.
//
// The rendering is the only place a request is constructed, which is what
// makes "the client cannot submit arbitrary SQL, a path, or a connector
// request" a property of the code rather than a convention: there is no
// parameter to put one in.
func DefectAggregateQuery(req DefectAggregateRequest) (url.Values, error) {
	gaggle := strings.TrimSpace(req.Gaggle)
	if gaggle == "" {
		return nil, ErrEndpointWithoutGaggle
	}
	if err := ValidateScopeName("gaggle", gaggle); err != nil {
		return nil, err
	}
	workflow := strings.TrimSpace(req.Workflow)
	if workflow != "" {
		if err := ValidateScopeName("workflow", workflow); err != nil {
			return nil, err
		}
	}
	if err := ValidateThresholds(req.Thresholds); err != nil {
		return nil, err
	}
	aggregates := req.Aggregates
	if len(aggregates) == 0 {
		aggregates = AdmittedAggregates()
	}
	normalized := make([]Aggregate, 0, len(aggregates))
	for _, aggregate := range aggregates {
		parsed, err := ParseAggregate(string(aggregate))
		if err != nil {
			return nil, err
		}
		if !containsAggregate(normalized, parsed) {
			normalized = append(normalized, parsed)
		}
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i] < normalized[j] })

	values := url.Values{}
	values.Set("gaggle", gaggle)
	if workflow != "" {
		values.Set("workflow", workflow)
	}
	values.Set("since", req.Since.UTC().Format(time.RFC3339Nano))
	values.Set("aggregates", JoinAggregates(normalized))
	setCount := func(name string, value int) {
		if value > 0 {
			values.Set(name, strconv.Itoa(value))
		}
	}
	setRate := func(name string, value float64) {
		if value > 0 {
			values.Set(name, strconv.FormatFloat(value, 'g', -1, 64))
		}
	}
	setCount("minSamples", req.Thresholds.MinSamples)
	setRate("maxFailureRate", req.Thresholds.MaxFailureRate)
	setCount("minErrorSignatureCount", req.Thresholds.MinErrorSignatureCount)
	setCount("minGateEvaluations", req.Thresholds.MinGateEvaluations)
	setRate("maxGateEscalationRate", req.Thresholds.MaxGateEscalationRate)
	setCount("maxFlaggedRuns", req.Thresholds.MaxFlaggedRuns)
	setCount("minCreditRuns", req.Thresholds.MinCreditRuns)
	setRate("minCreditFailureShare", req.Thresholds.MinCreditFailureShare)
	return values, nil
}

func containsAggregate(set []Aggregate, want Aggregate) bool {
	for _, aggregate := range set {
		if aggregate == want {
			return true
		}
	}
	return false
}

// scopeName is what a gaggle or workflow name may look like on this plane: a
// plain, single-segment identifier. It refuses path separators, traversal
// segments, wildcards, and anything a filesystem or a query would treat
// specially — the same fail-closed shape internal/httpapi applies to a gaggle
// path segment, applied here so a malformed scope never leaves the pod.
var scopeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateScopeName refuses a gaggle or workflow name that is not a plain
// single-segment identifier.
func ValidateScopeName(field, value string) error {
	if strings.Contains(value, "..") || !scopeName.MatchString(value) {
		return fmt.Errorf("telemetryclient: %s %q is not a plain name", field, value)
	}
	return nil
}

// DefectAggregates reads the four admitted aggregate families for this
// client's own gaggle.
//
// The gaggle is this client's, never the caller's: a request that names a
// different one is refused here rather than sent to be refused there, which
// is the same containment journalclient applies to its own run id.
func (h *HTTP) DefectAggregates(ctx context.Context, req DefectAggregateRequest) (DefectAggregateResponse, error) {
	if scope := strings.TrimSpace(req.Gaggle); scope != "" && scope != h.cfg.Gaggle {
		return DefectAggregateResponse{}, fmt.Errorf(
			"telemetryclient: a defect-aggregate read is contained to this stage's own gaggle; %q was requested", scope)
	}
	req.Gaggle = h.cfg.Gaggle
	if err := ValidateWindow(req.Since, time.Now()); err != nil {
		return DefectAggregateResponse{}, err
	}
	values, err := DefectAggregateQuery(req)
	if err != nil {
		return DefectAggregateResponse{}, err
	}
	target := h.cfg.BaseURL + apicontract.TelemetryDefectAggregatesPath + "?" + values.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return DefectAggregateResponse{}, fmt.Errorf("telemetryclient: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	request.Header.Set("Accept", "application/json")

	response, err := h.cfg.Client.Do(request)
	if err != nil {
		return DefectAggregateResponse{}, fmt.Errorf("telemetryclient: read defect aggregates: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return DefectAggregateResponse{}, planeError(response)
	}
	// Read under a hard ceiling rather than streaming the decode: a hostile
	// or broken endpoint must not be able to exhaust a stage pod's memory,
	// and an answer at the ceiling is a refusal, not a truncated result the
	// stage would act on.
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return DefectAggregateResponse{}, fmt.Errorf("telemetryclient: read defect aggregates: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return DefectAggregateResponse{}, fmt.Errorf(
			"telemetryclient: defect-aggregate response exceeds the %d byte ceiling", maxResponseBytes)
	}
	var decoded DefectAggregateResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return DefectAggregateResponse{}, fmt.Errorf("telemetryclient: decode defect aggregates: %w", err)
	}
	if decoded.Gaggle != "" && decoded.Gaggle != h.cfg.Gaggle {
		return DefectAggregateResponse{}, fmt.Errorf(
			"telemetryclient: defect aggregates for %q answered for gaggle %q", h.cfg.Gaggle, decoded.Gaggle)
	}
	if len(decoded.Findings) > MaxFindings {
		return DefectAggregateResponse{}, fmt.Errorf(
			"telemetryclient: defect-aggregate response carries %d findings, above the %d ceiling",
			len(decoded.Findings), MaxFindings)
	}
	if decoded.PromotionCandidates == nil {
		decoded.PromotionCandidates = []PromotionSignal{}
	}
	if decoded.Findings == nil {
		decoded.Findings = []Finding{}
	}
	return decoded, nil
}
