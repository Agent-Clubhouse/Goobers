package readservice

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/telemetry/rollup"
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
// model and harness-version cohort dimensions.
type TelemetryStatsRequest struct {
	Workflow              string
	Gaggle                string
	Model                 string
	HarnessVersion        string
	GroupByModel          bool
	GroupByHarnessVersion bool
	Since                 time.Time
	Until                 time.Time
}

// TelemetryStatsResult contains deterministic workflow and stage aggregates.
type TelemetryStatsResult struct {
	Gaggles []TelemetryGaggleStats `json:"gaggles"`
	Runs    []TelemetryRunStats    `json:"runs"`
	Stages  []TelemetryStageStats  `json:"stages"`
	Models  []TelemetryModelStats  `json:"models"`
}

// TelemetryGaggleStats is the run aggregate for one gaggle.
type TelemetryGaggleStats struct {
	Gaggle        string   `json:"gaggle"`
	TotalRuns     int      `json:"totalRuns"`
	CompletedRuns int      `json:"completedRuns"`
	FailedRuns    int      `json:"failedRuns"`
	OtherRuns     int      `json:"otherRuns"`
	SuccessRate   *float64 `json:"successRate,omitempty"`
	AvgDurationMs *float64 `json:"avgDurationMs,omitempty"`
	MinDurationMs *int64   `json:"minDurationMs,omitempty"`
	MaxDurationMs *int64   `json:"maxDurationMs,omitempty"`
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
}

// TelemetryStageStats is the attempt aggregate for one stage.
type TelemetryStageStats struct {
	Gaggle               string   `json:"gaggle"`
	Workflow             string   `json:"workflow"`
	Stage                string   `json:"stage"`
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
	Stats(rollup.StatsRequest) (rollup.StatsResult, error)
	TopErrorSignatures(rollup.StatsRequest, int) ([]rollup.ErrorSignature, error)
	Errors(rollup.ErrorsRequest) ([]rollup.ErrorEvent, error)
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
	if err := ctx.Err(); err != nil {
		return TelemetryStatsResult{}, err
	}
	stats, err := s.store.Stats(rollup.StatsRequest{
		Workflow:              req.Workflow,
		Gaggle:                req.Gaggle,
		Model:                 req.Model,
		HarnessVersion:        req.HarnessVersion,
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
		Gaggles: make([]TelemetryGaggleStats, 0, len(stats.Gaggles)),
		Runs:    make([]TelemetryRunStats, 0, len(stats.Runs)),
		Stages:  make([]TelemetryStageStats, 0, len(stats.Stages)),
		Models:  make([]TelemetryModelStats, 0, len(stats.Models)),
	}
	for _, stat := range stats.Gaggles {
		item := TelemetryGaggleStats{
			Gaggle:        stat.Gaggle,
			TotalRuns:     stat.TotalRuns,
			CompletedRuns: stat.CompletedRuns,
			FailedRuns:    stat.FailedRuns,
			OtherRuns:     stat.OtherRuns,
		}
		if stat.CompletedRuns+stat.FailedRuns > 0 {
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
			Gaggle:         stat.Gaggle,
			Workflow:       stat.Workflow,
			Model:          stat.Model,
			HarnessVersion: stat.HarnessVersion,
			TotalRuns:      stat.TotalRuns,
			CompletedRuns:  stat.CompletedRuns,
			FailedRuns:     stat.FailedRuns,
			OtherRuns:      stat.OtherRuns,
		}
		if stat.CompletedRuns+stat.FailedRuns > 0 {
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
			Gaggle:             stat.Gaggle,
			Workflow:           stat.Workflow,
			Stage:              stat.Stage,
			Model:              stat.Model,
			HarnessVersion:     stat.HarnessVersion,
			TotalAttempts:      stat.TotalAttempts,
			SucceededAttempts:  stat.SucceededAttempts,
			FailedAttempts:     stat.FailedAttempts,
			DurationSamples:    stat.DurationSamples,
			TokenSamples:       stat.TokenSamples,
			CostSamples:        stat.CostSamples,
			RetryWasteAttempts: stat.RetryWasteAttempts,
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
	signatures, err := s.store.TopErrorSignatures(rollup.StatsRequest{
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
	events, err := s.store.Errors(rollup.ErrorsRequest{
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
	return s.telemetry.TelemetryStats(ctx, req)
}

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

func float64Pointer(value float64) *float64 { return &value }
func int64Pointer(value int64) *int64       { return &value }
