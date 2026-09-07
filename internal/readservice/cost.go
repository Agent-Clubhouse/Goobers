package readservice

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const (
	// TelemetryCostScopeSummary returns both pull-request and issue aggregates.
	TelemetryCostScopeSummary = "summary"
	// TelemetryCostScopePullRequest returns one pull-request aggregate.
	TelemetryCostScopePullRequest = rollup.CostExternalKindPR
	// TelemetryCostScopeIssue returns one issue aggregate.
	TelemetryCostScopeIssue = rollup.CostExternalKindIssue
	// MaxTelemetryCostWindow bounds every cost query.
	MaxTelemetryCostWindow = 90 * 24 * time.Hour
)

// TelemetryCostRequest selects one bounded aggregate read.
type TelemetryCostRequest struct {
	Provider   string
	Scope      string
	ExternalID string
	Since      time.Time
	Until      time.Time
}

// TelemetryCostResult is the shared API, Portal, and CLI aggregate contract.
type TelemetryCostResult struct {
	Provider     string                   `json:"provider,omitempty"`
	Scope        string                   `json:"scope"`
	ExternalID   string                   `json:"externalId,omitempty"`
	Since        time.Time                `json:"since"`
	Until        time.Time                `json:"until"`
	PullRequests []TelemetryCostAggregate `json:"pullRequests"`
	Issues       []TelemetryCostAggregate `json:"issues"`
}

// TelemetryCostAggregate is one external work item's measured usage.
type TelemetryCostAggregate struct {
	Provider               string                        `json:"provider"`
	ExternalKind           string                        `json:"externalKind"`
	ExternalID             string                        `json:"externalId"`
	TotalRuns              int                           `json:"totalRuns"`
	MeasuredRuns           int                           `json:"measuredRuns"`
	TotalAttempts          int                           `json:"totalAttempts"`
	MeasuredAttempts       int                           `json:"measuredAttempts"`
	InputTokens            *int64                        `json:"inputTokens,omitempty"`
	OutputTokens           *int64                        `json:"outputTokens,omitempty"`
	CacheReadTokens        *int64                        `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens       *int64                        `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens        *int64                        `json:"reasoningTokens,omitempty"`
	CopilotPremiumRequests *float64                      `json:"copilotPremiumRequests,omitempty"`
	NativeTotals           []TelemetryCostAmount         `json:"nativeTotals"`
	NormalizedTotals       []TelemetryCostAmount         `json:"normalizedTotals"`
	BillingModels          []string                      `json:"billingModels"`
	CostBases              []string                      `json:"costBases"`
	Coverage               TelemetryCostCoverage         `json:"coverage"`
	Models                 []TelemetryCostModelAggregate `json:"models"`
	Runs                   []TelemetryCostRunAggregate   `json:"runs"`
}

// TelemetryCostModelAggregate preserves model-level native and normalized cost.
type TelemetryCostModelAggregate struct {
	Model                  string                `json:"model"`
	UsageAttempts          int                   `json:"usageAttempts"`
	MeasuredAttempts       int                   `json:"measuredAttempts"`
	InputTokens            *int64                `json:"inputTokens,omitempty"`
	OutputTokens           *int64                `json:"outputTokens,omitempty"`
	CacheReadTokens        *int64                `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens       *int64                `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens        *int64                `json:"reasoningTokens,omitempty"`
	CopilotPremiumRequests *float64              `json:"copilotPremiumRequests,omitempty"`
	NativeTotals           []TelemetryCostAmount `json:"nativeTotals"`
	NormalizedTotals       []TelemetryCostAmount `json:"normalizedTotals"`
	BillingModels          []string              `json:"billingModels"`
	CostBases              []string              `json:"costBases"`
}

// TelemetryCostRunAggregate preserves the run-level attribution beneath an
// external work item.
type TelemetryCostRunAggregate struct {
	RunID                  string                        `json:"runId"`
	StartedAt              time.Time                     `json:"startedAt"`
	UsageAttempts          int                           `json:"usageAttempts"`
	MeasuredAttempts       int                           `json:"measuredAttempts"`
	InputTokens            *int64                        `json:"inputTokens,omitempty"`
	OutputTokens           *int64                        `json:"outputTokens,omitempty"`
	CacheReadTokens        *int64                        `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens       *int64                        `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens        *int64                        `json:"reasoningTokens,omitempty"`
	CopilotPremiumRequests *float64                      `json:"copilotPremiumRequests,omitempty"`
	NativeTotals           []TelemetryCostAmount         `json:"nativeTotals"`
	NormalizedTotals       []TelemetryCostAmount         `json:"normalizedTotals"`
	BillingModels          []string                      `json:"billingModels"`
	CostBases              []string                      `json:"costBases"`
	Models                 []TelemetryCostModelAggregate `json:"models"`
}

// TelemetryCostAmount names a unit explicitly so clients never guess whether
// a number is provider-native or normalized.
type TelemetryCostAmount struct {
	Unit      string  `json:"unit"`
	Value     float64 `json:"value"`
	Estimated bool    `json:"estimated"`
}

// TelemetryCostCoverage makes partial measurement an explicit lower-bound
// condition rather than silently treating missing usage as zero.
type TelemetryCostCoverage struct {
	TotalRuns        int  `json:"totalRuns"`
	MeasuredRuns     int  `json:"measuredRuns"`
	TotalAttempts    int  `json:"totalAttempts"`
	MeasuredAttempts int  `json:"measuredAttempts"`
	Complete         bool `json:"complete"`
	LowerBound       bool `json:"lowerBound"`
}

// TelemetryCosts validates and projects the store aggregate.
func (s *Telemetry) TelemetryCosts(ctx context.Context, req TelemetryCostRequest) (TelemetryCostResult, error) {
	req.Provider = strings.TrimSpace(req.Provider)
	req.Scope = strings.TrimSpace(req.Scope)
	req.ExternalID = strings.TrimSpace(req.ExternalID)
	if req.Scope == "" {
		req.Scope = TelemetryCostScopeSummary
	}
	if err := validateTelemetryCostRequest(req); err != nil {
		return TelemetryCostResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TelemetryCostResult{}, err
	}
	query := rollup.CostQuery{
		Provider: req.Provider, Since: req.Since, Until: req.Until,
	}
	if req.Scope != TelemetryCostScopeSummary {
		query.ExternalKind = req.Scope
		query.ExternalID = req.ExternalID
	}
	costs, err := s.store.CostAggregates(ctx, query)
	if err != nil {
		return TelemetryCostResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TelemetryCostResult{}, err
	}
	result := TelemetryCostResult{
		Provider: req.Provider, Scope: req.Scope, ExternalID: req.ExternalID,
		Since: req.Since, Until: req.Until,
		PullRequests: make([]TelemetryCostAggregate, 0, len(costs.PullRequests)),
		Issues:       make([]TelemetryCostAggregate, 0, len(costs.Issues)),
	}
	for _, aggregate := range costs.PullRequests {
		result.PullRequests = append(result.PullRequests, projectCostAggregate(aggregate))
	}
	for _, aggregate := range costs.Issues {
		result.Issues = append(result.Issues, projectCostAggregate(aggregate))
	}
	return result, nil
}

func validateTelemetryCostRequest(req TelemetryCostRequest) error {
	if req.Scope != TelemetryCostScopeSummary &&
		req.Scope != TelemetryCostScopePullRequest &&
		req.Scope != TelemetryCostScopeIssue {
		return fmt.Errorf("%w: scope must be summary, pr, or issue", ErrInvalidTelemetryRequest)
	}
	if req.Since.IsZero() || req.Until.IsZero() || !req.Since.Before(req.Until) {
		return fmt.Errorf("%w: since and until must form an increasing bounded window", ErrInvalidTelemetryRequest)
	}
	if req.Until.Sub(req.Since) > MaxTelemetryCostWindow {
		return fmt.Errorf("%w: cost window must not exceed %s", ErrInvalidTelemetryRequest, MaxTelemetryCostWindow)
	}
	if req.Scope == TelemetryCostScopeSummary && req.ExternalID != "" {
		return fmt.Errorf("%w: summary does not accept an external id", ErrInvalidTelemetryRequest)
	}
	if req.Scope != TelemetryCostScopeSummary && req.ExternalID == "" {
		return fmt.Errorf("%w: pr and issue queries require an external id", ErrInvalidTelemetryRequest)
	}
	return nil
}

func projectCostAggregate(source rollup.CostAggregate) TelemetryCostAggregate {
	item := TelemetryCostAggregate{
		Provider: source.Provider, ExternalKind: source.ExternalKind, ExternalID: source.ExternalID,
		TotalRuns: source.TotalRuns, MeasuredRuns: source.MeasuredRuns,
		TotalAttempts: source.TotalAttempts, MeasuredAttempts: source.MeasuredAttempts,
		InputTokens: source.InputTokens, OutputTokens: source.OutputTokens,
		CacheReadTokens: source.CacheReadTokens, CacheWriteTokens: source.CacheWriteTokens,
		ReasoningTokens: source.ReasoningTokens, CopilotPremiumRequests: source.CopilotPremiumRequests,
		BillingModels: append([]string(nil), source.BillingModels...),
		CostBases:     append([]string(nil), source.CostBases...),
		Coverage:      costCoverage(source.TotalRuns, source.MeasuredRuns, source.TotalAttempts, source.MeasuredAttempts),
		Models:        make([]TelemetryCostModelAggregate, 0, len(source.Models)),
		Runs:          make([]TelemetryCostRunAggregate, 0, len(source.Runs)),
	}
	for _, model := range source.Models {
		item.Models = append(item.Models, projectCostModel(model))
	}
	item.NativeTotals = aggregateNativeCostTotals(item.Models)
	if len(item.NativeTotals) == 0 {
		item.NativeTotals = nativeCostTotals(source.NanoAIU, source.CostUSD, source.CopilotPremiumRequests, source.BillingModels, source.CostBases)
	}
	item.NormalizedTotals = normalizedCostTotals(source.NanoAIU, item.NativeTotals)
	for _, run := range source.Runs {
		item.Runs = append(item.Runs, projectCostRun(run))
	}
	return item
}

func projectCostModel(source rollup.CostModelAggregate) TelemetryCostModelAggregate {
	native := nativeCostTotals(source.NanoAIU, source.CostUSD, source.CopilotPremiumRequests, source.BillingModels, source.CostBases)
	return TelemetryCostModelAggregate{
		Model: source.Model, UsageAttempts: source.UsageAttempts, MeasuredAttempts: source.MeasuredAttempts,
		InputTokens: source.InputTokens, OutputTokens: source.OutputTokens,
		CacheReadTokens: source.CacheReadTokens, CacheWriteTokens: source.CacheWriteTokens,
		ReasoningTokens: source.ReasoningTokens, CopilotPremiumRequests: source.CopilotPremiumRequests,
		NativeTotals:     native,
		NormalizedTotals: normalizedCostTotals(source.NanoAIU, native),
		BillingModels:    append([]string(nil), source.BillingModels...),
		CostBases:        append([]string(nil), source.CostBases...),
	}
}

func projectCostRun(source rollup.CostRunAggregate) TelemetryCostRunAggregate {
	models := make([]TelemetryCostModelAggregate, 0, len(source.Models))
	for _, model := range source.Models {
		models = append(models, projectCostModel(model))
	}
	native := aggregateNativeCostTotals(models)
	if len(native) == 0 {
		native = nativeCostTotals(source.NanoAIU, source.CostUSD, source.CopilotPremiumRequests, source.BillingModels, source.CostBases)
	}
	return TelemetryCostRunAggregate{
		RunID: source.RunID, StartedAt: source.StartedAt,
		UsageAttempts: source.UsageAttempts, MeasuredAttempts: source.MeasuredAttempts,
		InputTokens: source.InputTokens, OutputTokens: source.OutputTokens,
		CacheReadTokens: source.CacheReadTokens, CacheWriteTokens: source.CacheWriteTokens,
		ReasoningTokens: source.ReasoningTokens, CopilotPremiumRequests: source.CopilotPremiumRequests,
		NativeTotals: native, NormalizedTotals: normalizedCostTotals(source.NanoAIU, native),
		BillingModels: append([]string(nil), source.BillingModels...),
		CostBases:     append([]string(nil), source.CostBases...), Models: models,
	}
}

func nativeCostTotals(nanoAIU *int64, costUSD, premium *float64, billingModels, costBases []string) []TelemetryCostAmount {
	estimated := slices.Contains(costBases, telemetry.CostBasisUnknown)
	var totals []TelemetryCostAmount
	if nanoAIU != nil && slices.Contains(billingModels, telemetry.BillingModelAICredits) {
		totals = append(totals, TelemetryCostAmount{
			Unit: "aiCredits", Value: float64(*nanoAIU) / float64(telemetry.NanoAIUPerAICredit), Estimated: estimated,
		})
	}
	if premium != nil && slices.Contains(billingModels, telemetry.BillingModelPremiumRequests) {
		totals = append(totals, TelemetryCostAmount{Unit: "premiumRequests", Value: *premium, Estimated: estimated})
	}
	if costUSD != nil && !slices.Contains(billingModels, telemetry.BillingModelAICredits) {
		totals = append(totals, TelemetryCostAmount{Unit: "usd", Value: *costUSD, Estimated: estimated})
	}
	return totals
}

func aggregateNativeCostTotals(models []TelemetryCostModelAggregate) []TelemetryCostAmount {
	values := map[string]float64{}
	estimated := map[string]bool{}
	for _, model := range models {
		for _, amount := range model.NativeTotals {
			values[amount.Unit] += amount.Value
			estimated[amount.Unit] = estimated[amount.Unit] || amount.Estimated
		}
	}
	var totals []TelemetryCostAmount
	for _, unit := range []string{"aiCredits", "usd", "premiumRequests"} {
		if value, ok := values[unit]; ok {
			totals = append(totals, TelemetryCostAmount{Unit: unit, Value: value, Estimated: estimated[unit]})
		}
	}
	return totals
}

func normalizedCostTotals(nanoAIU *int64, native []TelemetryCostAmount) []TelemetryCostAmount {
	if nanoAIU == nil {
		return []TelemetryCostAmount{}
	}
	hasAICredits := false
	hasUSD := false
	for _, amount := range native {
		hasAICredits = hasAICredits || amount.Unit == "aiCredits"
		hasUSD = hasUSD || amount.Unit == "usd"
	}
	if hasAICredits && !hasUSD {
		return []TelemetryCostAmount{{
			Unit: "usd", Value: float64(*nanoAIU) / float64(telemetry.NanoAIUPerUSD),
			Estimated: true,
		}}
	}
	if hasUSD && !hasAICredits {
		return []TelemetryCostAmount{{
			Unit: "aiCredits", Value: float64(*nanoAIU) / float64(telemetry.NanoAIUPerAICredit),
			Estimated: true,
		}}
	}
	return []TelemetryCostAmount{{
		Unit: "usd", Value: float64(*nanoAIU) / float64(telemetry.NanoAIUPerUSD),
		Estimated: true,
	}}
}

func costCoverage(totalRuns, measuredRuns, totalAttempts, measuredAttempts int) TelemetryCostCoverage {
	complete := totalRuns == measuredRuns && totalAttempts == measuredAttempts
	return TelemetryCostCoverage{
		TotalRuns: totalRuns, MeasuredRuns: measuredRuns,
		TotalAttempts: totalAttempts, MeasuredAttempts: measuredAttempts,
		Complete: complete, LowerBound: !complete,
	}
}

// TelemetryCosts implements TelemetryReader for the daemon's full local service.
func (s *Local) TelemetryCosts(ctx context.Context, req TelemetryCostRequest) (TelemetryCostResult, error) {
	if s.telemetry == nil {
		return TelemetryCostResult{}, ErrTelemetryUnavailable
	}
	return s.telemetry.TelemetryCosts(ctx, req)
}
