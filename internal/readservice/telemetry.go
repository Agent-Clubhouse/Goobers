package readservice

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
)

var (
	// ErrTelemetryUnavailable means telemetry is disabled for this instance.
	ErrTelemetryUnavailable = errors.New("telemetry is unavailable")
	// ErrInvalidTelemetryRequest identifies invalid filters or cursors.
	ErrInvalidTelemetryRequest = errors.New("invalid telemetry request")
)

// TelemetryReader is the shared telemetry read boundary used by HTTP and CLI.
type TelemetryReader interface {
	TelemetryStats(context.Context, TelemetryStatsRequest) (TelemetryStatsResult, error)
	TelemetryErrorSignatures(context.Context, TelemetryErrorSignaturesRequest) (TelemetryErrorSignaturesResult, error)
	TelemetryErrors(context.Context, TelemetryErrorsRequest) (TelemetryErrorsPage, error)
}

// TelemetryStatsRequest filters workflow/stage aggregates and selects optional
// branch, model, and harness-version cohort dimensions.
type TelemetryStatsRequest struct {
	Workflow              string
	Gaggle                string
	Branch                *int
	Model                 string
	HarnessVersion        string
	GroupByBranch         bool
	GroupByModel          bool
	GroupByHarnessVersion bool
	Since                 time.Time
	Until                 time.Time
}

// TelemetryStatsResult contains deterministic workflow and stage aggregates.
type TelemetryStatsResult struct {
	Gaggles          []TelemetryGaggleStats       `json:"gaggles"`
	Runs             []TelemetryRunStats          `json:"runs"`
	Stages           []TelemetryStageStats        `json:"stages"`
	Usage            []TelemetryUsageStats        `json:"usage"`
	Models           []TelemetryModelStats        `json:"models"`
	CreditAssignment []NodeCredit                 `json:"creditAssignment"`
	CausalCredit     []readmodel.CausalNodeCredit `json:"causalCredit"`
	GraphAnalytics   *readmodel.GraphAnalytics    `json:"graphAnalytics,omitempty"`
	PromotionSignals []PromotionSignal            `json:"promotionSignals,omitempty"`
	// PromotionCandidates is the machine-filtered input for automated
	// promotion. Correlational fallbacks remain visible in PromotionSignals
	// but never cross this boundary.
	PromotionCandidates []PromotionSignal      `json:"promotionCandidates,omitempty"`
	Curation            TelemetryCurationStats `json:"curation"`
	ReadyPool           TelemetryReadyPool     `json:"readyPool"`
}

// PromotionSignal is the bounded evidence interface for automated promotion.
type PromotionSignal struct {
	Node              string  `json:"node"`
	Value             float64 `json:"value"`
	Lower             float64 `json:"lower,omitempty"`
	Upper             float64 `json:"upper,omitempty"`
	Source            string  `json:"source"`
	Caveat            string  `json:"caveat"`
	PromotionEligible bool    `json:"promotionEligible"`
}

// EligiblePromotionSignals returns only identified causal signals with a
// confidence interval. Promotion consumers must use this boundary instead of
// interpreting fallback signals themselves.
func EligiblePromotionSignals(signals []PromotionSignal) []PromotionSignal {
	eligible := make([]PromotionSignal, 0, len(signals))
	for _, signal := range signals {
		if signal.PromotionEligible &&
			signal.Source != "correlational-fallback" &&
			!math.IsNaN(signal.Value) && !math.IsInf(signal.Value, 0) &&
			!math.IsNaN(signal.Lower) && !math.IsInf(signal.Lower, 0) &&
			!math.IsNaN(signal.Upper) && !math.IsInf(signal.Upper, 0) &&
			signal.Lower <= signal.Value && signal.Value <= signal.Upper {
			eligible = append(eligible, signal)
		}
	}
	return eligible
}

// NodeCredit ranks one workflow node's accumulated contribution to adverse
// outcomes over the requested telemetry window.
type NodeCredit struct {
	Gaggle             string   `json:"gaggle"`
	Workflow           string   `json:"workflow"`
	Kind               string   `json:"kind"`
	Stage              string   `json:"stage"`
	Identity           string   `json:"identity,omitempty"`
	RoutedRuns         int      `json:"routedRuns"`
	FailureRuns        int      `json:"failureRuns"`
	FailureShare       float64  `json:"failureShare"`
	EscalationRuns     int      `json:"escalationRuns"`
	RetryWasteAttempts int      `json:"retryWasteAttempts"`
	Effect             *float64 `json:"effect,omitempty"`
	Lower              *float64 `json:"lower,omitempty"`
	Upper              *float64 `json:"upper,omitempty"`
	Identification     string   `json:"identification"`
	Caveat             string   `json:"caveat,omitempty"`
}

// TelemetryCurationStats is the windowed action rollup for backlog curation.
// EverRecorded distinguishes a curation workflow that has never once
// invoked its telemetry writers (#2278) from one that simply has no
// action rows in the requested window — Ready/NeedsHuman/Closed/etc and
// ForwardCurationThroughput otherwise show as real zeros in both cases.
type TelemetryCurationStats struct {
	EverRecorded bool `json:"everRecorded"`
	Runs         int  `json:"runs"`
	ReportedRuns int  `json:"reportedRuns"`
	Ready        int  `json:"ready"`
	NeedsHuman   int  `json:"needsHuman"`
	Closed       int  `json:"closed"`
	Deduped      int  `json:"deduped"`
	Split        int  `json:"split"`
	Stale        int  `json:"stale"`
	Reconciled   int  `json:"reconciled"`
	Milestoned   int  `json:"milestoned"`
	Bounced      int  `json:"bounced"`
}

// TelemetryReadyPool is the latest ready-pool snapshot plus windowed quality
// and flow measures. SampleEverRecorded/BounceEverRecorded mirror
// TelemetryCurationStats.EverRecorded for the ready-pool-sample-backed
// fields (Depth/AverageAgeSeconds/OldestAgeSeconds/Starved) and the
// ready-label-transition-backed field (BounceRate) respectively — both of
// those fields are already nil-when-absent, but nil is ambiguous between
// "never recorded" and "no data in the selected window" without this.
type TelemetryReadyPool struct {
	SampleEverRecorded        bool       `json:"sampleEverRecorded"`
	ObservedAt                *time.Time `json:"observedAt,omitempty"`
	Depth                     *int       `json:"depth,omitempty"`
	AverageAgeSeconds         *float64   `json:"averageAgeSeconds,omitempty"`
	OldestAgeSeconds          *float64   `json:"oldestAgeSeconds,omitempty"`
	Starved                   *bool      `json:"starved,omitempty"`
	ClaimAgeSamples           int        `json:"claimAgeSamples"`
	AverageClaimAgeSeconds    *float64   `json:"averageClaimAgeSeconds,omitempty"`
	BounceEverRecorded        bool       `json:"bounceEverRecorded"`
	BounceRate                *float64   `json:"bounceRate,omitempty"`
	ForwardCurationThroughput int        `json:"forwardCurationThroughput"`
	ImplementationDemand      int        `json:"implementationDemand"`
	// InFlightClaimSamples/AverageInFlightClaimAgeSeconds/
	// OldestInFlightClaimAgeSeconds report how long currently open
	// implementation claims have been claimed as of now (#2279) — distinct
	// from AverageClaimAgeSeconds, which reports the completed
	// ready-to-claim transition. Zero is always a real, meaningful value
	// (no open claims), so these are plain fields, not omitempty pointers.
	InFlightClaimSamples           int     `json:"inFlightClaimSamples"`
	AverageInFlightClaimAgeSeconds float64 `json:"averageInFlightClaimAgeSeconds"`
	OldestInFlightClaimAgeSeconds  float64 `json:"oldestInFlightClaimAgeSeconds"`
}

// TelemetryGaggleStats is the run aggregate for one gaggle.
type TelemetryGaggleStats struct {
	Gaggle        string `json:"gaggle"`
	TotalRuns     int    `json:"totalRuns"`
	CompletedRuns int    `json:"completedRuns"`
	FailedRuns    int    `json:"failedRuns"`
	// InfraFailedRuns is how many of FailedRuns terminated on an
	// infrastructure fault rather than a verdict about the work, and are
	// therefore excluded from SuccessRate's denominator (#3361/#3364).
	InfraFailedRuns int      `json:"infraFailedRuns"`
	OtherRuns       int      `json:"otherRuns"`
	SuccessRate     *float64 `json:"successRate,omitempty"`
	AvgDurationMs   *float64 `json:"avgDurationMs,omitempty"`
	MinDurationMs   *int64   `json:"minDurationMs,omitempty"`
	MaxDurationMs   *int64   `json:"maxDurationMs,omitempty"`
}

// TelemetryRunStats is the run aggregate for one workflow. Optional metrics
// are absent when no matching run has produced the underlying measurement.
type TelemetryRunStats struct {
	Gaggle         string   `json:"gaggle"`
	Workflow       string   `json:"workflow"`
	Model          string   `json:"model,omitempty"`
	HarnessVersion string   `json:"harnessVersion,omitempty"`
	TotalRuns      int      `json:"totalRuns"`
	CompletedRuns  int      `json:"completedRuns"`
	FailedRuns     int      `json:"failedRuns"`
	OtherRuns      int      `json:"otherRuns"`
	SuccessRate    *float64 `json:"successRate,omitempty"`
	AvgDurationMs  *float64 `json:"avgDurationMs,omitempty"`
	MinDurationMs  *int64   `json:"minDurationMs,omitempty"`
	MaxDurationMs  *int64   `json:"maxDurationMs,omitempty"`
	// InfraFailedRuns is how many of FailedRuns terminated on an
	// infrastructure fault (credential materialization, git, network, lock
	// contention) rather than a verdict about the work, and are therefore
	// excluded from SuccessRate's denominator (#3361/#3364).
	InfraFailedRuns int `json:"infraFailedRuns"`
	// StuckAbortedRuns is how many of TotalRuns were excluded from
	// Avg/Min/MaxDurationMs because they hung and were later aborted (the
	// watchdog's max-duration expiry) rather than reaching a designed
	// terminal — disclosed rather than silently dropped (#2534, #1439).
	StuckAbortedRuns int `json:"stuckAbortedRuns"`
}

// TelemetryStageStats is the attempt aggregate for one stage.
type TelemetryStageStats struct {
	Gaggle               string   `json:"gaggle"`
	Workflow             string   `json:"workflow"`
	Stage                string   `json:"stage"`
	Branch               *int     `json:"branch,omitempty"`
	Model                string   `json:"model,omitempty"`
	HarnessVersion       string   `json:"harnessVersion,omitempty"`
	TotalAttempts        int      `json:"totalAttempts"`
	SucceededAttempts    int      `json:"succeededAttempts"`
	FailedAttempts       int      `json:"failedAttempts"`
	SuccessRate          *float64 `json:"successRate,omitempty"`
	AvgDurationMs        *float64 `json:"avgDurationMs,omitempty"`
	MinDurationMs        *int64   `json:"minDurationMs,omitempty"`
	MaxDurationMs        *int64   `json:"maxDurationMs,omitempty"`
	DurationSamples      int      `json:"durationSamples"`
	P50DurationMs        *int64   `json:"p50DurationMs,omitempty"`
	P95DurationMs        *int64   `json:"p95DurationMs,omitempty"`
	TokenSamples         int      `json:"tokenSamples"`
	P50Tokens            *int64   `json:"p50Tokens,omitempty"`
	P95Tokens            *int64   `json:"p95Tokens,omitempty"`
	CostSamples          int      `json:"costSamples"`
	P50CostUSD           *float64 `json:"p50CostUSD,omitempty"`
	P95CostUSD           *float64 `json:"p95CostUSD,omitempty"`
	RetryWasteAttempts   int      `json:"retryWasteAttempts"`
	RetryWasteDurationMs *int64   `json:"retryWasteDurationMs,omitempty"`
	RetryWasteTokens     *int64   `json:"retryWasteTokens,omitempty"`
	RetryWasteCostUSD    *float64 `json:"retryWasteCostUSD,omitempty"`
	// StuckAbortedAttempts is how many of TotalAttempts belong to a run that
	// hung and was later aborted (the watchdog's max-duration expiry),
	// excluded from Avg/Min/MaxDurationMs and from P50/P95DurationMs —
	// disclosed rather than silently dropped (#2534, #1439).
	StuckAbortedAttempts int `json:"stuckAbortedAttempts"`
}

// TelemetryUsageStats is an exact AI usage rollup for one operational scope.
type TelemetryUsageStats struct {
	Scope                     string   `json:"scope"`
	Gaggle                    string   `json:"gaggle,omitempty"`
	Workflow                  string   `json:"workflow,omitempty"`
	Stage                     string   `json:"stage,omitempty"`
	Branch                    *int     `json:"branch,omitempty"`
	Model                     string   `json:"model,omitempty"`
	HarnessVersion            string   `json:"harnessVersion,omitempty"`
	TotalAttempts             int      `json:"totalAttempts"`
	TokenSamples              int      `json:"tokenSamples"`
	P50Tokens                 *int64   `json:"p50Tokens,omitempty"`
	P95Tokens                 *int64   `json:"p95Tokens,omitempty"`
	PremiumRequestSamples     int      `json:"premiumRequestSamples"`
	P50CopilotPremiumRequests *float64 `json:"p50CopilotPremiumRequests,omitempty"`
	P95CopilotPremiumRequests *float64 `json:"p95CopilotPremiumRequests,omitempty"`
	CostSamples               int      `json:"costSamples"`
	CostUSD                   *float64 `json:"costUSD,omitempty"`
	P50CostUSD                *float64 `json:"p50CostUSD,omitempty"`
	P95CostUSD                *float64 `json:"p95CostUSD,omitempty"`
	RetryWasteAttempts        int      `json:"retryWasteAttempts"`
	RetryWasteTokens          *int64   `json:"retryWasteTokens,omitempty"`
	RetryWasteCostUSD         *float64 `json:"retryWasteCostUSD,omitempty"`
}

// TelemetryModelStats is observed usage totaled by model.
type TelemetryModelStats struct {
	Model                  string   `json:"model"`
	UsageSamples           int      `json:"usageSamples"`
	InputTokenSamples      int      `json:"inputTokenSamples"`
	InputTokens            *int64   `json:"inputTokens,omitempty"`
	OutputTokenSamples     int      `json:"outputTokenSamples"`
	OutputTokens           *int64   `json:"outputTokens,omitempty"`
	PremiumRequestSamples  int      `json:"premiumRequestSamples"`
	CopilotPremiumRequests *float64 `json:"copilotPremiumRequests,omitempty"`
	CostSamples            int      `json:"costSamples"`
	CostUSD                *float64 `json:"costUSD,omitempty"`
}

// TelemetryErrorSignaturesRequest filters the recurring failure-reason rollup.
type TelemetryErrorSignaturesRequest struct {
	Workflow string
	Gaggle   string
	Stage    string
	Since    time.Time
	Until    time.Time
	Limit    int
}

// TelemetryErrorSignaturesResult contains recurring errors ordered by frequency.
type TelemetryErrorSignaturesResult struct {
	Items []TelemetryErrorSignature `json:"items"`
}

// TelemetryErrorSignature is one recurring code and coarse error-class pair.
type TelemetryErrorSignature struct {
	Code           string    `json:"code"`
	ErrorClass     string    `json:"errorClass"`
	Count          int       `json:"count"`
	LastSeen       time.Time `json:"lastSeen"`
	ExampleRunID   string    `json:"exampleRunId,omitempty"`
	ExampleStage   string    `json:"exampleStage,omitempty"`
	ExampleAttempt int       `json:"exampleAttempt,omitempty"`
}

// TelemetryErrorsRequest filters and paginates recent errors.
type TelemetryErrorsRequest struct {
	Workflow         string
	Gaggle           string
	Stage            string
	Code             string
	ErrorClass       string
	FilterCode       bool
	FilterErrorClass bool
	Since            time.Time
	Until            time.Time
	Limit            int
	Cursor           string
}

// TelemetryErrorsPage contains newest-first error events and an opaque cursor.
type TelemetryErrorsPage struct {
	Items      []TelemetryError `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// TelemetryError is one error with its run and stage references.
type TelemetryError struct {
	RunID      string    `json:"runId"`
	Workflow   string    `json:"workflow"`
	Stage      string    `json:"stage"`
	Attempt    int       `json:"attempt"`
	Code       string    `json:"code"`
	ErrorClass string    `json:"errorClass"`
	Message    string    `json:"message"`
	OccurredAt time.Time `json:"occurredAt"`
}

type telemetryStore interface {
	Stats(context.Context, rollup.StatsRequest) (rollup.StatsResult, error)
	TopErrorSignatures(context.Context, rollup.StatsRequest, int) ([]rollup.ErrorSignature, error)
	Errors(context.Context, rollup.ErrorsRequest) ([]rollup.ErrorEvent, error)
}

// Telemetry projects the telemetry rollup into the shared read contract.
type Telemetry struct {
	store telemetryStore
}

// NewTelemetry constructs an in-process telemetry read service for CLI use.
func NewTelemetry(db *rollup.DB) (*Telemetry, error) {
	if db == nil {
		return nil, ErrTelemetryUnavailable
	}
	return &Telemetry{store: db}, nil
}

// TelemetryStats returns workflow and stage aggregates in stable name order.
func (s *Telemetry) TelemetryStats(ctx context.Context, req TelemetryStatsRequest) (TelemetryStatsResult, error) {
	if err := validateWindow(req.Since, req.Until); err != nil {
		return TelemetryStatsResult{}, err
	}
	if req.Branch != nil && *req.Branch < 0 {
		return TelemetryStatsResult{}, fmt.Errorf("%w: branch must be non-negative", ErrInvalidTelemetryRequest)
	}
	if err := ctx.Err(); err != nil {
		return TelemetryStatsResult{}, err
	}
	stats, err := s.store.Stats(ctx, rollup.StatsRequest{
		Workflow:              req.Workflow,
		Gaggle:                req.Gaggle,
		Branch:                req.Branch,
		Model:                 req.Model,
		HarnessVersion:        req.HarnessVersion,
		GroupByBranch:         req.GroupByBranch,
		GroupByModel:          req.GroupByModel,
		GroupByHarnessVersion: req.GroupByHarnessVersion,
		Since:                 req.Since,
		Until:                 req.Until,
	})
	if err != nil {
		return TelemetryStatsResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TelemetryStatsResult{}, err
	}

	result := TelemetryStatsResult{
		Gaggles:          make([]TelemetryGaggleStats, 0, len(stats.Gaggles)),
		Runs:             make([]TelemetryRunStats, 0, len(stats.Runs)),
		Stages:           make([]TelemetryStageStats, 0, len(stats.Stages)),
		Usage:            make([]TelemetryUsageStats, 0, len(stats.Usage)),
		Models:           make([]TelemetryModelStats, 0, len(stats.Models)),
		CreditAssignment: []NodeCredit{},
		Curation: TelemetryCurationStats{
			EverRecorded: stats.Curation.EverRecorded,
			Runs:         stats.Curation.Runs,
			ReportedRuns: stats.Curation.ReportedRuns,
			Ready:        stats.Curation.Ready,
			NeedsHuman:   stats.Curation.NeedsHuman,
			Closed:       stats.Curation.Closed,
			Deduped:      stats.Curation.Deduped,
			Split:        stats.Curation.Split,
			Stale:        stats.Curation.Stale,
			Reconciled:   stats.Curation.Reconciled,
			Milestoned:   stats.Curation.Milestoned,
			Bounced:      stats.Curation.Bounced,
		},
		ReadyPool: TelemetryReadyPool{
			SampleEverRecorded:             stats.ReadyPool.SampleEverRecorded,
			BounceEverRecorded:             stats.ReadyPool.BounceEverRecorded,
			ClaimAgeSamples:                stats.ReadyPool.ClaimAgeSamples,
			ForwardCurationThroughput:      stats.ReadyPool.ForwardCurationThroughput,
			ImplementationDemand:           stats.ReadyPool.ImplementationDemand,
			InFlightClaimSamples:           stats.ReadyPool.InFlightClaimSamples,
			AverageInFlightClaimAgeSeconds: stats.ReadyPool.AverageInFlightClaimAgeSeconds,
			OldestInFlightClaimAgeSeconds:  stats.ReadyPool.OldestInFlightClaimAgeSeconds,
		},
	}
	if stats.ReadyPool.HasSample {
		result.ReadyPool.ObservedAt = timePointer(stats.ReadyPool.ObservedAt)
		result.ReadyPool.Depth = intPointer(stats.ReadyPool.Depth)
		result.ReadyPool.AverageAgeSeconds = float64Pointer(stats.ReadyPool.AverageAgeSeconds)
		result.ReadyPool.OldestAgeSeconds = float64Pointer(stats.ReadyPool.OldestAgeSeconds)
		result.ReadyPool.Starved = boolPointer(stats.ReadyPool.Starved)
	}
	if stats.ReadyPool.ClaimAgeSamples > 0 {
		result.ReadyPool.AverageClaimAgeSeconds = float64Pointer(stats.ReadyPool.AverageClaimAgeSeconds)
	}
	if stats.ReadyPool.HasBounceRate {
		result.ReadyPool.BounceRate = float64Pointer(stats.ReadyPool.BounceRate)
	}
	for _, stat := range stats.Gaggles {
		item := TelemetryGaggleStats{
			Gaggle:          stat.Gaggle,
			TotalRuns:       stat.TotalRuns,
			CompletedRuns:   stat.CompletedRuns,
			FailedRuns:      stat.FailedRuns,
			InfraFailedRuns: stat.InfraFailedRuns,
			OtherRuns:       stat.OtherRuns,
		}
		// The rate is absent, not zero, when nothing reached a verdict about
		// the work — an all-infra-fault window makes no claim either way.
		if stat.CompletedRuns+stat.FailedRuns-stat.InfraFailedRuns > 0 {
			item.SuccessRate = float64Pointer(stat.SuccessRate)
		}
		if stat.HasDuration {
			item.AvgDurationMs = float64Pointer(stat.AvgDurationMs)
			item.MinDurationMs = int64Pointer(stat.MinDurationMs)
			item.MaxDurationMs = int64Pointer(stat.MaxDurationMs)
		}
		result.Gaggles = append(result.Gaggles, item)
	}
	for _, stat := range stats.Runs {
		item := TelemetryRunStats{
			Gaggle:           stat.Gaggle,
			Workflow:         stat.Workflow,
			Model:            stat.Model,
			HarnessVersion:   stat.HarnessVersion,
			TotalRuns:        stat.TotalRuns,
			CompletedRuns:    stat.CompletedRuns,
			FailedRuns:       stat.FailedRuns,
			InfraFailedRuns:  stat.InfraFailedRuns,
			OtherRuns:        stat.OtherRuns,
			StuckAbortedRuns: stat.StuckAbortedRuns,
		}
		if stat.CompletedRuns+stat.FailedRuns-stat.InfraFailedRuns > 0 {
			item.SuccessRate = float64Pointer(stat.SuccessRate)
		}
		if stat.HasDuration {
			item.AvgDurationMs = float64Pointer(stat.AvgDurationMs)
			item.MinDurationMs = int64Pointer(stat.MinDurationMs)
			item.MaxDurationMs = int64Pointer(stat.MaxDurationMs)
		}
		result.Runs = append(result.Runs, item)
	}
	for _, stat := range stats.Stages {
		item := TelemetryStageStats{
			Gaggle:               stat.Gaggle,
			Workflow:             stat.Workflow,
			Stage:                stat.Stage,
			Branch:               stat.Branch,
			Model:                stat.Model,
			HarnessVersion:       stat.HarnessVersion,
			TotalAttempts:        stat.TotalAttempts,
			SucceededAttempts:    stat.SucceededAttempts,
			FailedAttempts:       stat.FailedAttempts,
			DurationSamples:      stat.DurationSamples,
			TokenSamples:         stat.TokenSamples,
			CostSamples:          stat.CostSamples,
			RetryWasteAttempts:   stat.RetryWasteAttempts,
			StuckAbortedAttempts: stat.StuckAbortedAttempts,
		}
		if stat.SucceededAttempts+stat.FailedAttempts > 0 {
			item.SuccessRate = float64Pointer(stat.SuccessRate)
		}
		if stat.HasDuration {
			item.AvgDurationMs = float64Pointer(stat.AvgDurationMs)
			item.MinDurationMs = int64Pointer(stat.MinDurationMs)
			item.MaxDurationMs = int64Pointer(stat.MaxDurationMs)
			item.P50DurationMs = int64Pointer(stat.P50DurationMs)
			item.P95DurationMs = int64Pointer(stat.P95DurationMs)
		}
		if stat.HasTokens {
			item.P50Tokens = int64Pointer(stat.P50Tokens)
			item.P95Tokens = int64Pointer(stat.P95Tokens)
		}
		if stat.HasCost {
			item.P50CostUSD = float64Pointer(stat.P50CostUSD)
			item.P95CostUSD = float64Pointer(stat.P95CostUSD)
		}
		if stat.HasRetryWasteDuration {
			item.RetryWasteDurationMs = int64Pointer(stat.RetryWasteDurationMs)
		}
		if stat.HasRetryWasteTokens {
			item.RetryWasteTokens = int64Pointer(stat.RetryWasteTokens)
		}
		if stat.HasRetryWasteCost {
			item.RetryWasteCostUSD = float64Pointer(stat.RetryWasteCostUSD)
		}
		result.Stages = append(result.Stages, item)
	}
	for _, stat := range stats.Usage {
		item := TelemetryUsageStats{
			Scope:                 stat.Scope,
			Gaggle:                stat.Gaggle,
			Workflow:              stat.Workflow,
			Stage:                 stat.Stage,
			Branch:                stat.Branch,
			Model:                 stat.Model,
			HarnessVersion:        stat.HarnessVersion,
			TotalAttempts:         stat.TotalAttempts,
			TokenSamples:          stat.TokenSamples,
			PremiumRequestSamples: stat.PremiumRequestSamples,
			CostSamples:           stat.CostSamples,
			RetryWasteAttempts:    stat.RetryWasteAttempts,
		}
		if stat.HasTokens {
			item.P50Tokens = int64Pointer(stat.P50Tokens)
			item.P95Tokens = int64Pointer(stat.P95Tokens)
		}
		if stat.HasPremiumRequests {
			item.P50CopilotPremiumRequests = float64Pointer(stat.P50CopilotPremiumRequests)
			item.P95CopilotPremiumRequests = float64Pointer(stat.P95CopilotPremiumRequests)
		}
		if stat.HasCost {
			item.CostUSD = float64Pointer(stat.CostUSD)
			item.P50CostUSD = float64Pointer(stat.P50CostUSD)
			item.P95CostUSD = float64Pointer(stat.P95CostUSD)
		}
		if stat.HasRetryWasteTokens {
			item.RetryWasteTokens = int64Pointer(stat.RetryWasteTokens)
		}
		if stat.HasRetryWasteCost {
			item.RetryWasteCostUSD = float64Pointer(stat.RetryWasteCostUSD)
		}
		result.Usage = append(result.Usage, item)
	}
	for _, stat := range stats.Models {
		item := TelemetryModelStats{
			Model:                 stat.Model,
			UsageSamples:          stat.UsageSamples,
			InputTokenSamples:     stat.InputTokenSamples,
			OutputTokenSamples:    stat.OutputTokenSamples,
			PremiumRequestSamples: stat.PremiumRequestSamples,
			CostSamples:           stat.CostSamples,
		}
		if stat.HasInputTokens {
			item.InputTokens = int64Pointer(stat.InputTokens)
		}
		if stat.HasOutputTokens {
			item.OutputTokens = int64Pointer(stat.OutputTokens)
		}
		if stat.HasPremiumRequests {
			item.CopilotPremiumRequests = float64Pointer(stat.CopilotPremiumRequests)
		}
		if stat.HasCost {
			item.CostUSD = float64Pointer(stat.CostUSD)
		}
		result.Models = append(result.Models, item)
	}
	return result, nil
}

// TelemetryErrorSignatures returns recurring failure reasons in frequency order.
func (s *Telemetry) TelemetryErrorSignatures(ctx context.Context, req TelemetryErrorSignaturesRequest) (TelemetryErrorSignaturesResult, error) {
	if err := validateWindow(req.Since, req.Until); err != nil {
		return TelemetryErrorSignaturesResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TelemetryErrorSignaturesResult{}, err
	}
	signatures, err := s.store.TopErrorSignatures(ctx, rollup.StatsRequest{
		Workflow: req.Workflow,
		Gaggle:   req.Gaggle,
		Stage:    req.Stage,
		Since:    req.Since,
		Until:    req.Until,
	}, req.Limit)
	if err != nil {
		return TelemetryErrorSignaturesResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TelemetryErrorSignaturesResult{}, err
	}

	result := TelemetryErrorSignaturesResult{
		Items: make([]TelemetryErrorSignature, 0, len(signatures)),
	}
	for _, signature := range signatures {
		result.Items = append(result.Items, TelemetryErrorSignature{
			Code:           signature.Code,
			ErrorClass:     signature.ErrorClass,
			Count:          signature.Count,
			LastSeen:       signature.LastSeen,
			ExampleRunID:   signature.ExampleRunID,
			ExampleStage:   signature.ExampleStage,
			ExampleAttempt: signature.ExampleAttempt,
		})
	}
	return result, nil
}

// TelemetryErrors returns one deterministic page of newest-first errors.
func (s *Telemetry) TelemetryErrors(ctx context.Context, req TelemetryErrorsRequest) (TelemetryErrorsPage, error) {
	if err := validateWindow(req.Since, req.Until); err != nil {
		return TelemetryErrorsPage{}, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	queryLimit := limit
	maxInt := int(^uint(0) >> 1)
	if queryLimit < maxInt {
		queryLimit++
	}
	cursor, err := decodeErrorsCursor(req)
	if err != nil {
		return TelemetryErrorsPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return TelemetryErrorsPage{}, err
	}
	events, err := s.store.Errors(ctx, rollup.ErrorsRequest{
		Workflow:         req.Workflow,
		Gaggle:           req.Gaggle,
		Stage:            req.Stage,
		Code:             req.Code,
		ErrorClass:       req.ErrorClass,
		FilterCode:       req.FilterCode,
		FilterErrorClass: req.FilterErrorClass,
		Since:            req.Since,
		Until:            req.Until,
		Limit:            queryLimit,
		Cursor:           cursor,
	})
	if err != nil {
		return TelemetryErrorsPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return TelemetryErrorsPage{}, err
	}

	hasNext := queryLimit > limit && len(events) > limit
	if hasNext {
		events = events[:limit]
	}
	page := TelemetryErrorsPage{Items: make([]TelemetryError, 0, len(events))}
	for _, event := range events {
		page.Items = append(page.Items, TelemetryError{
			RunID:      event.RunID,
			Workflow:   event.Workflow,
			Stage:      event.Stage,
			Attempt:    event.Attempt,
			Code:       event.Code,
			ErrorClass: event.ErrorClass,
			Message:    event.Message,
			OccurredAt: event.OccurredAt,
		})
	}
	if hasNext {
		page.NextCursor, err = encodeErrorsCursor(events[len(events)-1], req)
		if err != nil {
			return TelemetryErrorsPage{}, err
		}
	}
	return page, nil
}

// TelemetryStats implements TelemetryReader for the daemon's full local service.
func (s *Local) TelemetryStats(ctx context.Context, req TelemetryStatsRequest) (TelemetryStatsResult, error) {
	if s.telemetry == nil {
		return TelemetryStatsResult{}, ErrTelemetryUnavailable
	}
	result, err := s.telemetry.TelemetryStats(ctx, req)
	if err != nil || s.sources.ReadModel == nil {
		return result, err
	}
	credits, err := s.sources.ReadModel.CreditAssignment(ctx, readmodel.CreditOptions{
		Gaggle: req.Gaggle, Workflow: req.Workflow, Since: req.Since, Until: req.Until,
	})
	if err != nil {
		return TelemetryStatsResult{}, err
	}
	result.CreditAssignment = make([]NodeCredit, 0, len(credits))
	for _, credit := range credits {
		failureShare := 0.0
		if credit.RoutedRuns > 0 {
			failureShare = float64(credit.FailureRuns) / float64(credit.RoutedRuns)
		}
		result.CreditAssignment = append(result.CreditAssignment, NodeCredit{
			Gaggle: credit.Gaggle, Workflow: credit.Workflow, Kind: credit.Kind,
			Stage: credit.Stage, Identity: credit.Identity,
			RoutedRuns: credit.RoutedRuns, FailureRuns: credit.FailureRuns,
			FailureShare: failureShare, EscalationRuns: credit.EscalationRuns,
			RetryWasteAttempts: credit.RetryWasteAttempts,
		})
	}
	causal, err := s.sources.ReadModel.CausalCredit(ctx, readmodel.CausalOptions{
		Gaggle:        req.Gaggle,
		Workflow:      req.Workflow,
		Since:         req.Since,
		Until:         req.Until,
		WorkflowGraph: getWorkflowGraphForQuery(s.definitionsForQuery(), req.Gaggle, req.Workflow),
	})
	if err != nil {
		return TelemetryStatsResult{}, err
	}
	result.CausalCredit = causal
	causalByNode := make(map[string]readmodel.CausalNodeCredit, len(causal))
	for _, estimate := range causal {
		causalByNode[estimate.Node] = estimate
	}
	result.PromotionSignals = make([]PromotionSignal, 0, len(result.CreditAssignment))
	for i := range result.CreditAssignment {
		credit := &result.CreditAssignment[i]
		estimate, ok := causalByNode[credit.Kind+":"+credit.Stage]
		if !ok || estimate.Identification == readmodel.CausalUnidentifiable {
			credit.Identification = "correlational-fallback"
			credit.Caveat = "no identified causal intervention; correlational rollup retained"
			result.PromotionSignals = append(result.PromotionSignals, PromotionSignal{
				Node: credit.Kind + ":" + credit.Stage, Value: credit.FailureShare,
				Source: "correlational-fallback", Caveat: credit.Caveat,
			})
			continue
		}
		credit.Identification = string(estimate.Identification)
		credit.Caveat = estimate.Caveat
		if estimate.IntervalAvailable && estimate.PromotionEligible {
			credit.Effect = float64Ptr(estimate.Effect)
			credit.Lower = float64Ptr(estimate.Lower)
			credit.Upper = float64Ptr(estimate.Upper)
			result.PromotionSignals = append(result.PromotionSignals, PromotionSignal{
				Node: credit.Kind + ":" + credit.Stage, Value: estimate.Effect,
				Lower: estimate.Lower, Upper: estimate.Upper,
				Source: estimate.PromotionSource, Caveat: estimate.Caveat,
				PromotionEligible: true,
			})
		} else {
			credit.Identification = "correlational-fallback"
			credit.Caveat = "causal estimate has no promotion-eligible confidence interval; correlational rollup retained"
			result.PromotionSignals = append(result.PromotionSignals, PromotionSignal{
				Node: credit.Kind + ":" + credit.Stage, Value: credit.FailureShare,
				Source: "correlational-fallback", Caveat: credit.Caveat,
			})
		}
	}
	result.PromotionCandidates = EligiblePromotionSignals(result.PromotionSignals)
	if graph := getWorkflowGraphForQuery(s.definitionsForQuery(), req.Gaggle, req.Workflow); graph != nil {
		runtimeGraph, err := s.runtimeAnalyticsGraph(ctx, req, graph)
		if err != nil {
			return TelemetryStatsResult{}, err
		}
		analyticsGraph := readmodel.AnalyticsGraph{
			Nodes: make([]readmodel.AnalyticsNode, 0, len(runtimeGraph.Nodes)),
			Edges: make([]readmodel.AnalyticsEdge, 0, len(runtimeGraph.Edges)),
		}
		failureByNode, trustedFailure, creditNodes := normalizedPromotionFailure(
			result.CreditAssignment, result.PromotionCandidates,
		)
		latencyByNode := make(map[string]float64, len(result.Stages))
		for _, stage := range result.Stages {
			if stage.Workflow == req.Workflow && stage.AvgDurationMs != nil {
				latencyByNode[stage.Stage] = *stage.AvgDurationMs
			}
		}
		for _, node := range runtimeGraph.Nodes {
			analyticsGraph.Nodes = append(analyticsGraph.Nodes, readmodel.AnalyticsNode{
				ID: node.ID, Failure: failureByNode[node.ID], Latency: latencyByNode[node.ID],
			})
		}
		for _, edge := range runtimeGraph.Edges {
			analyticsGraph.Edges = append(analyticsGraph.Edges, readmodel.AnalyticsEdge{
				Source: edge.Source, Target: edge.Target,
			})
		}
		analytics, err := readmodel.AnalyzeGraph(analyticsGraph)
		if err != nil {
			return TelemetryStatsResult{}, err
		}
		if len(trustedFailure) == 0 {
			analytics.Centrality = nil
			analytics.CriticalPath = readmodel.CriticalPath{}
			analytics.Confidence = "untrusted"
			analytics.Caveat = "centrality and critical path are withheld because no promotion-eligible causal confidence interval is available"
		} else if !sameAnalyticsNodes(trustedFailure, creditNodes) {
			analytics.Confidence = "partial"
			analytics.Caveat = "centrality uses only promotion-eligible causal weights; correlational fallbacks are excluded"
		} else {
			analytics.Confidence = "bounded"
		}
		result.GraphAnalytics = &analytics
	}
	return result, nil
}

// normalizedPromotionFailure reconciles identity-level credits with the
// stage-level causal signals used by the graph.
func normalizedPromotionFailure(credits []NodeCredit, signals []PromotionSignal) (map[string]float64, map[string]bool, map[string]bool) {
	creditNodes := make(map[string]bool, len(credits))
	for _, credit := range credits {
		creditNodes[normalizeAnalyticsNode(credit.Kind+":"+credit.Stage)] = true
	}

	type aggregate struct {
		total float64
		count int
	}
	aggregates := make(map[string]aggregate, len(signals))
	for _, signal := range signals {
		node := normalizeAnalyticsNode(signal.Node)
		item := aggregates[node]
		item.total += signal.Value
		item.count++
		aggregates[node] = item
	}
	failureByNode := make(map[string]float64, len(aggregates))
	trustedFailure := make(map[string]bool, len(aggregates))
	for node, item := range aggregates {
		if item.count == 0 {
			continue
		}
		failureByNode[node] = item.total / float64(item.count)
		trustedFailure[node] = true
	}
	return failureByNode, trustedFailure, creditNodes
}

func sameAnalyticsNodes(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for node := range left {
		if !right[node] {
			return false
		}
	}
	return true
}

func normalizeAnalyticsNode(node string) string {
	node = strings.TrimPrefix(node, "stage:")
	return strings.TrimPrefix(node, "gate:")
}

func (s *Local) definitionsForQuery() *instance.ConfigSet {
	if snapshot := s.definitions.Load(); snapshot != nil {
		return snapshot.set
	}
	return s.sources.Definitions
}

// runtimeAnalyticsGraph overlays the declared topology with the transitions
// actually observed across runs. The declared workflow is a DAG, while repass
// and cross-run review/CI transitions are allowed to form SCCs.
func (s *Local) runtimeAnalyticsGraph(ctx context.Context, req TelemetryStatsRequest, declared *workflow.Graph) (readmodel.AnalyticsGraph, error) {
	graph := readmodel.AnalyticsGraph{}
	if declared != nil {
		for _, node := range declared.Nodes {
			graph.Nodes = append(graph.Nodes, readmodel.AnalyticsNode{ID: node.ID})
		}
		for _, edge := range declared.Edges {
			if edge.Target != "" && edge.Terminal == "" {
				graph.Edges = append(graph.Edges, readmodel.AnalyticsEdge{Source: edge.Source, Target: edge.Target})
			}
		}
	}
	ids, err := s.RunIDs(ctx)
	if err != nil {
		return readmodel.AnalyticsGraph{}, err
	}
	known := make(map[string]bool, len(graph.Nodes))
	for _, node := range graph.Nodes {
		known[node.ID] = true
	}
	edges := make(map[string]bool, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges[edge.Source+"\x00"+edge.Target] = true
	}
	type runtimeRun struct {
		detail RunDetail
	}
	runs := make([]runtimeRun, 0, len(ids))
	for _, id := range ids {
		detail, err := s.GetRun(ctx, id)
		if err != nil {
			return readmodel.AnalyticsGraph{}, fmt.Errorf("read analytics run %q: %w", id, err)
		}
		if detail.Gaggle != req.Gaggle || detail.Workflow != req.Workflow ||
			(!req.Since.IsZero() && detail.StartedAt.Before(req.Since)) ||
			(!req.Until.IsZero() && !detail.StartedAt.Before(req.Until)) {
			continue
		}
		runs = append(runs, runtimeRun{detail: detail})
		for _, transition := range detail.Transitions {
			if transition.Source == "" || transition.Target == "" || transition.Terminal ||
				transition.Target == workflow.TargetAbort || transition.Target == workflow.TargetEscalate {
				continue
			}
			for _, node := range []string{transition.Source, transition.Target} {
				if !known[node] {
					known[node] = true
					graph.Nodes = append(graph.Nodes, readmodel.AnalyticsNode{ID: node})
				}
			}
			key := transition.Source + "\x00" + transition.Target
			if !edges[key] {
				edges[key] = true
				graph.Edges = append(graph.Edges, readmodel.AnalyticsEdge{Source: transition.Source, Target: transition.Target})
			}
		}
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].detail.StartedAt.Equal(runs[j].detail.StartedAt) {
			return runs[i].detail.ID < runs[j].detail.ID
		}
		return runs[i].detail.StartedAt.Before(runs[j].detail.StartedAt)
	})
	previousByIssue := make(map[string]RunDetail)
	for _, run := range runs {
		current := run.detail
		if current.Operator.Issue == nil || current.Operator.Issue.Number == "" {
			continue
		}
		previous, seen := previousByIssue[current.Operator.Issue.Number]
		previousByIssue[current.Operator.Issue.Number] = current
		if !seen || len(previous.Transitions) == 0 || len(current.Transitions) == 0 {
			continue
		}
		source := previous.Transitions[len(previous.Transitions)-1].Target
		if source == "" {
			source = previous.Transitions[len(previous.Transitions)-1].Source
		}
		target := current.Transitions[0].Source
		if target == "" {
			target = current.Transitions[0].Target
		}
		if source == "" || target == "" ||
			workflow.IsReservedTarget(source) || workflow.IsReservedTarget(target) {
			continue
		}
		for _, node := range []string{source, target} {
			if !known[node] {
				known[node] = true
				graph.Nodes = append(graph.Nodes, readmodel.AnalyticsNode{ID: node})
			}
		}
		key := source + "\x00" + target
		if !edges[key] {
			edges[key] = true
			graph.Edges = append(graph.Edges, readmodel.AnalyticsEdge{Source: source, Target: target})
		}
	}
	return graph, nil
}

// getWorkflowGraphForQuery returns the compiled workflow graph for a given gaggle/workflow pair.
// Returns nil if the workflow is not found or cannot be compiled.
func getWorkflowGraphForQuery(definitions *instance.ConfigSet, gaggle, workflowName string) *workflow.Graph {
	if definitions == nil || workflowName == "" {
		return nil
	}
	for _, w := range definitions.Workflows {
		if w.Spec.Gaggle == gaggle && w.Name == workflowName {
			def := workflow.Definition{
				Spec: w.Spec,
			}
			machine, err := workflow.Compile(def)
			if err != nil {
				return nil
			}
			graph := machine.Graph()
			return &graph
		}
	}
	return nil
}

func float64Ptr(value float64) *float64 { return &value }

// TelemetryErrorSignatures implements TelemetryReader for the daemon's full local service.
func (s *Local) TelemetryErrorSignatures(ctx context.Context, req TelemetryErrorSignaturesRequest) (TelemetryErrorSignaturesResult, error) {
	if s.telemetry == nil {
		return TelemetryErrorSignaturesResult{}, ErrTelemetryUnavailable
	}
	return s.telemetry.TelemetryErrorSignatures(ctx, req)
}

// TelemetryErrors implements TelemetryReader for the daemon's full local service.
func (s *Local) TelemetryErrors(ctx context.Context, req TelemetryErrorsRequest) (TelemetryErrorsPage, error) {
	if s.telemetry == nil {
		return TelemetryErrorsPage{}, ErrTelemetryUnavailable
	}
	return s.telemetry.TelemetryErrors(ctx, req)
}

func validateWindow(since, until time.Time) error {
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return fmt.Errorf("%w: since must not be after until", ErrInvalidTelemetryRequest)
	}
	return nil
}

type telemetryErrorsCursor struct {
	OccurredAt string `json:"occurredAt"`
	RunID      string `json:"runId"`
	Sequence   uint64 `json:"sequence"`
	Filter     string `json:"filter"`
}

func encodeErrorsCursor(event rollup.ErrorEvent, req TelemetryErrorsRequest) (string, error) {
	orderTimestamp := event.OrderTimestamp
	if orderTimestamp == "" && !event.OccurredAt.IsZero() {
		orderTimestamp = formatCursorTime(event.OccurredAt)
	}
	data, err := json.Marshal(telemetryErrorsCursor{
		OccurredAt: orderTimestamp,
		RunID:      event.RunID,
		Sequence:   event.Sequence,
		Filter:     telemetryErrorsFilter(req),
	})
	if err != nil {
		return "", fmt.Errorf("encode telemetry cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeErrorsCursor(req TelemetryErrorsRequest) (*rollup.ErrorCursor, error) {
	if req.Cursor == "" {
		return nil, nil
	}
	if len(req.Cursor) > 512 {
		return nil, fmt.Errorf("%w: cursor is invalid", ErrInvalidTelemetryRequest)
	}
	data, err := base64.RawURLEncoding.DecodeString(req.Cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor is invalid", ErrInvalidTelemetryRequest)
	}
	var cursor telemetryErrorsCursor
	if err := json.Unmarshal(data, &cursor); err != nil ||
		cursor.Sequence == 0 || cursor.Filter == "" {
		return nil, fmt.Errorf("%w: cursor is invalid", ErrInvalidTelemetryRequest)
	}
	if cursor.Filter != telemetryErrorsFilter(req) {
		return nil, fmt.Errorf("%w: cursor does not match the requested filters", ErrInvalidTelemetryRequest)
	}
	if cursor.OccurredAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, cursor.OccurredAt); err != nil {
			return nil, fmt.Errorf("%w: cursor is invalid", ErrInvalidTelemetryRequest)
		}
	}
	return &rollup.ErrorCursor{
		OrderTimestamp: cursor.OccurredAt,
		RunID:          cursor.RunID,
		Sequence:       cursor.Sequence,
	}, nil
}

func telemetryErrorsFilter(req TelemetryErrorsRequest) string {
	data, _ := json.Marshal(struct {
		Workflow         string `json:"workflow"`
		Gaggle           string `json:"gaggle"`
		Stage            string `json:"stage"`
		Code             string `json:"code"`
		ErrorClass       string `json:"errorClass"`
		FilterCode       bool   `json:"filterCode"`
		FilterErrorClass bool   `json:"filterErrorClass"`
		Since            string `json:"since"`
		Until            string `json:"until"`
	}{
		Workflow:         req.Workflow,
		Gaggle:           req.Gaggle,
		Stage:            req.Stage,
		Code:             req.Code,
		ErrorClass:       req.ErrorClass,
		FilterCode:       req.FilterCode,
		FilterErrorClass: req.FilterErrorClass,
		Since:            formatCursorTime(req.Since),
		Until:            formatCursorTime(req.Until),
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func formatCursorTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func float64Pointer(value float64) *float64  { return &value }
func int64Pointer(value int64) *int64        { return &value }
func intPointer(value int) *int              { return &value }
func boolPointer(value bool) *bool           { return &value }
func timePointer(value time.Time) *time.Time { return &value }
