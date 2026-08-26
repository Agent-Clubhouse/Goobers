package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeTelemetryStore struct {
	stats          rollup.StatsResult
	signatures     []rollup.ErrorSignature
	errors         []rollup.ErrorEvent
	err            error
	statsReq       rollup.StatsRequest
	signatureReq   rollup.StatsRequest
	signatureLimit int
	errorReqs      []rollup.ErrorsRequest
	statsCalled    int
	trendReq       rollup.TrendRequest
	trendResults   []rollup.TrendResult
	signatureCalls int
}

type analyticsReadModel struct {
	readmodel.Reader
	credits []readmodel.NodeCredit
	causal  []readmodel.CausalNodeCredit
}

func (r *analyticsReadModel) CreditAssignment(context.Context, readmodel.CreditOptions) ([]readmodel.NodeCredit, error) {
	return r.credits, nil
}

func (r *analyticsReadModel) CausalCredit(context.Context, readmodel.CausalOptions) ([]readmodel.CausalNodeCredit, error) {
	return r.causal, nil
}

func (f *fakeTelemetryStore) Stats(_ context.Context, req rollup.StatsRequest) (rollup.StatsResult, error) {
	f.statsCalled++
	f.statsReq = req
	return f.stats, f.err
}

func (f *fakeTelemetryStore) TrendStats(_ context.Context, req rollup.TrendRequest) ([]rollup.TrendResult, error) {
	f.trendReq = req
	return f.trendResults, f.err
}

func (f *fakeTelemetryStore) TopErrorSignatures(_ context.Context, req rollup.StatsRequest, limit int) ([]rollup.ErrorSignature, error) {
	f.signatureCalls++
	f.signatureReq = req
	f.signatureLimit = limit
	return f.signatures, f.err
}

func (f *fakeTelemetryStore) Errors(_ context.Context, req rollup.ErrorsRequest) ([]rollup.ErrorEvent, error) {
	f.errorReqs = append(f.errorReqs, req)
	if f.err != nil {
		return nil, f.err
	}
	start := 0
	if req.Cursor != nil {
		start = len(f.errors)
		for i, event := range f.errors {
			if event.RunID == req.Cursor.RunID && event.Sequence == req.Cursor.Sequence &&
				formatCursorTime(event.OccurredAt) == req.Cursor.OrderTimestamp {
				start = i + 1
				break
			}
		}
	}
	end := start + req.Limit
	if end > len(f.errors) {
		end = len(f.errors)
	}
	return f.errors[start:end], nil
}

func TestTelemetryStatsProjectsFiltersAndUnknownMetrics(t *testing.T) {
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	branch := 2
	store := &fakeTelemetryStore{stats: rollup.StatsResult{
		Gaggles: []rollup.GaggleStats{
			{Gaggle: "core", TotalRuns: 2, CompletedRuns: 1, FailedRuns: 1, SuccessRate: 0.5},
		},
		Runs: []rollup.RunStats{
			{Gaggle: "core", Workflow: "failed", Model: "gpt-5.6-sol", HarnessVersion: "copilot version 1.2.3", TotalRuns: 1, FailedRuns: 1, HasDuration: true},
			{Gaggle: "core", Workflow: "running", TotalRuns: 1, OtherRuns: 1},
		},
		Stages: []rollup.StageStats{
			{
				Gaggle: "core", Workflow: "failed", Stage: "done", Branch: &branch, Model: "gpt-5.6-sol", HarnessVersion: "copilot version 1.2.3", TotalAttempts: 2, FailedAttempts: 1, HasDuration: true,
				DurationSamples: 2, P50DurationMs: 10, P95DurationMs: 20,
				TokenSamples: 2, P50Tokens: 100, P95Tokens: 200, HasTokens: true,
				CostSamples: 2, P50CostUSD: 0.5, P95CostUSD: 1, HasCost: true,
				RetryWasteAttempts: 1, RetryWasteDurationMs: 10, HasRetryWasteDuration: true,
				RetryWasteTokens: 100, HasRetryWasteTokens: true,
				RetryWasteCostUSD: 0.5, HasRetryWasteCost: true,
			},
			{Gaggle: "core", Workflow: "running", Stage: "active", TotalAttempts: 1},
		},
		Usage: []rollup.UsageStats{{
			Scope: "workflow", Gaggle: "core", Workflow: "failed", Branch: &branch,
			TotalAttempts: 2,
			TokenSamples:  2, P50Tokens: 100, P95Tokens: 200, HasTokens: true,
			PremiumRequestSamples: 2, P50CopilotPremiumRequests: 0, P95CopilotPremiumRequests: 1, HasPremiumRequests: true,
			CostSamples: 2, CostUSD: 1.5, P50CostUSD: 0.5, P95CostUSD: 1, HasCost: true,
			RetryWasteAttempts: 1, RetryWasteTokens: 100, HasRetryWasteTokens: true,
			RetryWasteCostUSD: 0.5, HasRetryWasteCost: true,
		}},
		Models: []rollup.ModelStats{{
			Model: "gpt-5.4", UsageSamples: 1,
			InputTokenSamples: 1, InputTokens: 0, HasInputTokens: true,
		}},
		Curation: rollup.CurationStats{
			Runs: 2, ReportedRuns: 1, Ready: 4, NeedsHuman: 1, Bounced: 1,
		},
		ReadyPool: rollup.ReadyPoolHealth{
			HasSample: true, ObservedAt: since.Add(time.Hour), Depth: 0, Starved: true,
			AverageAgeSeconds: 3600, OldestAgeSeconds: 7200,
			ClaimAgeSamples: 2, AverageClaimAgeSeconds: 5400,
			HasBounceRate: true, BounceRate: 0.2,
			ForwardCurationThroughput: 4, ImplementationDemand: 3,
		},
	}}
	service := &Telemetry{store: store}

	got, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{
		Workflow:              "implement",
		Gaggle:                "core",
		Branch:                &branch,
		Model:                 "gpt-5.6-sol",
		HarnessVersion:        "copilot version 1.2.3",
		GroupByBranch:         true,
		GroupByModel:          true,
		GroupByHarnessVersion: true,
		Since:                 since,
		Until:                 until,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReq := rollup.StatsRequest{
		Workflow: "implement", Gaggle: "core", Branch: &branch, Model: "gpt-5.6-sol", HarnessVersion: "copilot version 1.2.3",
		GroupByBranch: true, GroupByModel: true, GroupByHarnessVersion: true, Since: since, Until: until,
	}
	if !reflect.DeepEqual(store.statsReq, wantReq) {
		t.Fatalf("store request = %+v, want %+v", store.statsReq, wantReq)
	}
	if len(got.Gaggles) != 1 || got.Gaggles[0].Gaggle != "core" ||
		got.Gaggles[0].SuccessRate == nil || *got.Gaggles[0].SuccessRate != 0.5 {
		t.Fatalf("projected gaggle stats = %+v", got.Gaggles)
	}
	if got.Runs[0].Gaggle != "core" || got.Runs[0].SuccessRate == nil || *got.Runs[0].SuccessRate != 0 {
		t.Fatalf("observed zero success rate = %v, want pointer to zero", got.Runs[0].SuccessRate)
	}
	if got.Runs[0].AvgDurationMs == nil || *got.Runs[0].AvgDurationMs != 0 {
		t.Fatalf("observed zero duration = %v, want pointer to zero", got.Runs[0].AvgDurationMs)
	}
	if got.Runs[1].SuccessRate != nil || got.Runs[1].AvgDurationMs != nil {
		t.Fatalf("running metrics = %+v, want unknown metrics absent", got.Runs[1])
	}
	if got.Stages[1].Gaggle != "core" || got.Stages[1].Workflow != "running" ||
		got.Stages[1].SuccessRate != nil || got.Stages[1].AvgDurationMs != nil {
		t.Fatalf("active stage metrics = %+v, want unknown metrics absent", got.Stages[1])
	}
	done := got.Stages[0]
	if got.Runs[0].Model != "gpt-5.6-sol" || got.Runs[0].HarnessVersion != "copilot version 1.2.3" ||
		done.Branch == nil || *done.Branch != 2 ||
		done.Model != "gpt-5.6-sol" || done.HarnessVersion != "copilot version 1.2.3" {
		t.Fatalf("projected provenance = %+v / %+v", got.Runs[0], done)
	}
	if done.P50DurationMs == nil || *done.P50DurationMs != 10 ||
		done.P95Tokens == nil || *done.P95Tokens != 200 ||
		done.P50CostUSD == nil || *done.P50CostUSD != 0.5 ||
		done.RetryWasteDurationMs == nil || *done.RetryWasteDurationMs != 10 ||
		done.RetryWasteTokens == nil || *done.RetryWasteTokens != 100 ||
		done.RetryWasteCostUSD == nil || *done.RetryWasteCostUSD != 0.5 {
		t.Fatalf("projected stage distributions = %+v", done)
	}
	if len(got.Models) != 1 ||
		got.Models[0].InputTokens == nil || *got.Models[0].InputTokens != 0 ||
		got.Models[0].OutputTokens != nil || got.Models[0].CostUSD != nil {
		t.Fatalf("projected model usage = %+v", got.Models)
	}
	if len(got.Usage) != 1 || got.Usage[0].Scope != "workflow" ||
		got.Usage[0].Branch == nil || *got.Usage[0].Branch != 2 ||
		got.Usage[0].P95Tokens == nil || *got.Usage[0].P95Tokens != 200 ||
		got.Usage[0].P50CopilotPremiumRequests == nil || *got.Usage[0].P50CopilotPremiumRequests != 0 ||
		got.Usage[0].P95CopilotPremiumRequests == nil || *got.Usage[0].P95CopilotPremiumRequests != 1 ||
		got.Usage[0].CostUSD == nil || *got.Usage[0].CostUSD != 1.5 ||
		got.Usage[0].RetryWasteCostUSD == nil || *got.Usage[0].RetryWasteCostUSD != 0.5 {
		t.Fatalf("projected scope usage = %+v", got.Usage)
	}
	if got.Curation.Runs != 2 || got.Curation.ReportedRuns != 1 || got.Curation.Ready != 4 {
		t.Fatalf("projected curation = %+v", got.Curation)
	}
	if got.ReadyPool.Depth == nil || *got.ReadyPool.Depth != 0 ||
		got.ReadyPool.Starved == nil || !*got.ReadyPool.Starved ||
		got.ReadyPool.AverageClaimAgeSeconds == nil || *got.ReadyPool.AverageClaimAgeSeconds != 5400 ||
		got.ReadyPool.BounceRate == nil || *got.ReadyPool.BounceRate != 0.2 {
		t.Fatalf("projected ready-pool health = %+v", got.ReadyPool)
	}

	data, err := json.Marshal(got.Runs[1])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"successRate", "avgDurationMs", "minDurationMs", "maxDurationMs"} {
		if _, ok := fields[name]; ok {
			t.Fatalf("unknown metric %q was serialized: %s", name, data)
		}
	}

	data, err = json.Marshal(got.Stages[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"successRate", "avgDurationMs", "minDurationMs", "maxDurationMs",
		"p50DurationMs", "p95DurationMs", "p50Tokens", "p95Tokens",
		"p50CostUSD", "p95CostUSD", "retryWasteDurationMs", "retryWasteTokens", "retryWasteCostUSD",
	} {
		if _, ok := fields[name]; ok {
			t.Fatalf("unknown stage metric %q was serialized: %s", name, data)
		}
	}
}

func TestTelemetryStatsEmptySlicesAndInvalidWindow(t *testing.T) {
	store := &fakeTelemetryStore{}
	service := &Telemetry{store: store}
	got, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{})
	if err != nil {
		t.Fatal(err)
	}

	if got.Gaggles == nil || got.Runs == nil || got.Stages == nil || got.Usage == nil || got.Models == nil ||
		len(got.Gaggles) != 0 || len(got.Runs) != 0 || len(got.Stages) != 0 || len(got.Usage) != 0 || len(got.Models) != 0 {
		t.Fatalf("empty stats = %#v", got)
	}

	now := time.Now()
	if _, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{
		Since: now,
		Until: now.Add(-time.Second),
	}); !errors.Is(err, ErrInvalidTelemetryRequest) {
		t.Fatalf("invalid window error = %v", err)
	}
	if store.statsCalled != 1 {
		t.Fatalf("store called %d times, want only the valid query", store.statsCalled)
	}
	negativeBranch := -1
	if _, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{Branch: &negativeBranch}); !errors.Is(err, ErrInvalidTelemetryRequest) {
		t.Fatalf("negative branch error = %v", err)
	}
}

func TestTelemetryStatsTrendUsesOneBatchedQueryAndPreservesWindows(t *testing.T) {
	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	previousSince := since.Add(-24 * time.Hour)
	previousUntil := since
	branch := 3
	store := &fakeTelemetryStore{
		trendResults: []rollup.TrendResult{
			{Usage: []rollup.UsageStats{{Scope: "instance", CostSamples: 1, CostUSD: 1, HasCost: true}}},
			{Usage: []rollup.UsageStats{{Scope: "instance", CostSamples: 1, CostUSD: 2, HasCost: true}}},
			{Usage: []rollup.UsageStats{{Scope: "instance", CostSamples: 1, CostUSD: 3, HasCost: true}}},
		},
	}
	service := &Telemetry{store: store}
	got, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{
		Gaggle: "core", Workflow: "implement", Branch: &branch,
		GroupByBranch: true, TrendSince: since, TrendUntil: until,
		TrendBuckets: 2, TrendPreviousSince: previousSince, TrendPreviousUntil: previousUntil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.statsCalled != 1 || len(store.trendReq.Windows) != 3 {
		t.Fatalf("stats calls = %d, trend windows = %d; want one base query and three windows",
			store.statsCalled, len(store.trendReq.Windows))
	}
	if !store.trendReq.Windows[0].Since.Equal(since) ||
		!store.trendReq.Windows[0].Until.Equal(since.Add(12*time.Hour)) ||
		!store.trendReq.Windows[1].Until.Equal(until) ||
		!store.trendReq.Windows[2].Since.Equal(previousSince) ||
		!store.trendReq.Windows[2].Until.Equal(previousUntil) {
		t.Fatalf("trend windows = %+v", store.trendReq.Windows)
	}
	if store.trendReq.Stats.Gaggle != "core" || store.trendReq.Stats.Workflow != "implement" ||
		store.trendReq.Stats.Branch == nil || *store.trendReq.Stats.Branch != branch ||
		!store.trendReq.Stats.GroupByBranch {
		t.Fatalf("trend filters = %+v", store.trendReq.Stats)
	}
	if len(got.Trend) != 2 || len(got.Trend[0].Usage) != 1 ||
		got.Trend[1].Usage[0].CostUSD == nil || *got.Trend[1].Usage[0].CostUSD != 2 ||
		got.TrendPrevious == nil || got.TrendPrevious.Usage[0].CostUSD == nil ||
		*got.TrendPrevious.Usage[0].CostUSD != 3 {
		t.Fatalf("trend projection = %+v, previous = %+v", got.Trend, got.TrendPrevious)
	}
}

func TestTelemetryErrorSignaturesProjectsScopeAndExamples(t *testing.T) {
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	store := &fakeTelemetryStore{signatures: []rollup.ErrorSignature{{
		Code:           "harness.crash",
		ErrorClass:     "unknown",
		Count:          3,
		LastSeen:       until,
		ExampleRunID:   "run-3",
		ExampleStage:   "review",
		ExampleAttempt: 2,
	}}}
	service := &Telemetry{store: store}

	got, err := service.TelemetryErrorSignatures(context.Background(), TelemetryErrorSignaturesRequest{
		Workflow: "implement",
		Gaggle:   "core",
		Stage:    "review",
		Since:    since,
		Until:    until,
		Limit:    12,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReq := rollup.StatsRequest{
		Workflow: "implement",
		Gaggle:   "core",
		Stage:    "review",
		Since:    since,
		Until:    until,
	}
	if !reflect.DeepEqual(store.signatureReq, wantReq) || store.signatureLimit != 12 {
		t.Fatalf("store request = %+v limit %d, want %+v limit 12", store.signatureReq, store.signatureLimit, wantReq)
	}
	if len(got.Items) != 1 ||
		got.Items[0].ErrorClass != "unknown" ||
		got.Items[0].ExampleRunID != "run-3" ||
		got.Items[0].ExampleStage != "review" {
		t.Fatalf("signatures = %+v", got.Items)
	}
}

func TestTelemetryErrorsPaginatesAndBindsCursorToFilters(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeTelemetryStore{errors: []rollup.ErrorEvent{
		{Sequence: 3, RunID: "3", Workflow: "implement", Code: "third", OccurredAt: base.Add(3 * time.Hour)},
		{Sequence: 2, RunID: "", Workflow: "", Code: "second", OccurredAt: base.Add(2 * time.Hour)},
		{Sequence: 1, RunID: "1", Workflow: "implement", Code: "first", OccurredAt: base.Add(time.Hour)},
	}}
	service := &Telemetry{store: store}
	req := TelemetryErrorsRequest{
		Workflow:         "implement",
		Gaggle:           "core",
		Stage:            "review",
		Code:             "harness.crash",
		ErrorClass:       "unknown",
		FilterCode:       true,
		FilterErrorClass: true,
		Since:            base,
		Until:            base.Add(4 * time.Hour),
		Limit:            2,
	}

	first, err := service.TelemetryErrors(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.Items[0].Code != "third" || first.Items[1].Code != "second" || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	if got := store.errorReqs[0]; got.Limit != 3 ||
		got.Cursor != nil ||
		got.Gaggle != req.Gaggle ||
		got.Stage != req.Stage ||
		got.Code != req.Code ||
		!got.FilterCode ||
		got.ErrorClass != req.ErrorClass ||
		!got.FilterErrorClass ||
		got.Until != req.Until {
		t.Fatalf("first store request = %+v", got)
	}

	req.Cursor = first.NextCursor
	second, err := service.TelemetryErrors(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Code != "first" || second.NextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
	if got := store.errorReqs[1]; got.Cursor == nil ||
		got.Cursor.RunID != "" || got.Cursor.Sequence != 2 ||
		got.Cursor.OrderTimestamp != formatCursorTime(base.Add(2*time.Hour)) {
		t.Fatalf("second store cursor = %+v", got.Cursor)
	}

	req.Workflow = "nominate"
	if _, err := service.TelemetryErrors(context.Background(), req); !errors.Is(err, ErrInvalidTelemetryRequest) {
		t.Fatalf("filter-mismatched cursor error = %v", err)
	}
	if len(store.errorReqs) != 2 {
		t.Fatalf("store received %d requests, want 2", len(store.errorReqs))
	}
}

func TestTelemetryQueriesHonorContextAndStoreErrors(t *testing.T) {
	storeErr := errors.New("query failed")
	service := &Telemetry{store: &fakeTelemetryStore{err: storeErr}}
	if _, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{}); !errors.Is(err, storeErr) {
		t.Fatalf("stats error = %v", err)
	}
	if _, err := service.TelemetryErrorSignatures(context.Background(), TelemetryErrorSignaturesRequest{}); !errors.Is(err, storeErr) {
		t.Fatalf("error signatures error = %v", err)
	}
	if _, err := service.TelemetryErrors(context.Background(), TelemetryErrorsRequest{}); !errors.Is(err, storeErr) {
		t.Fatalf("errors error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &fakeTelemetryStore{}
	service = &Telemetry{store: store}
	if _, err := service.TelemetryStats(ctx, TelemetryStatsRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stats error = %v", err)
	}
	if store.statsCalled != 0 {
		t.Fatalf("canceled query reached store %d times", store.statsCalled)
	}
	if _, err := service.TelemetryErrorSignatures(ctx, TelemetryErrorSignaturesRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error signatures error = %v", err)
	}
	if store.signatureCalls != 0 {
		t.Fatalf("canceled query reached signature store %d times", store.signatureCalls)
	}
}

func TestLocalTelemetryUnavailable(t *testing.T) {
	service, err := NewLocal(LocalSources{Definitions: testDefinitions()}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{}); !errors.Is(err, ErrTelemetryUnavailable) {
		t.Fatalf("stats error = %v", err)
	}
	if _, err := service.TelemetryErrorSignatures(context.Background(), TelemetryErrorSignaturesRequest{}); !errors.Is(err, ErrTelemetryUnavailable) {
		t.Fatalf("error signatures error = %v", err)
	}
	if _, err := service.TelemetryErrors(context.Background(), TelemetryErrorsRequest{}); !errors.Is(err, ErrTelemetryUnavailable) {
		t.Fatalf("errors error = %v", err)
	}
}

func TestLocalTelemetryStatsProjectsCausalAndFallbackIdentification(t *testing.T) {
	ctx := context.Background()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), readmodel.FileName))
	if err != nil {
		t.Fatalf("open read model: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 8, 22, 4, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		seedLocalTelemetryRun(t, store, "control", "sha256:v1", start.Add(time.Duration(i)*time.Minute), false)
		seedLocalTelemetryRun(t, store, "treatment", "sha256:v2", start.Add(time.Duration(20+i)*time.Minute), true)
	}
	seedLocalTelemetryOtherNode(t, store, start.Add(50*time.Minute))

	service := &Local{
		telemetry: &Telemetry{store: &fakeTelemetryStore{
			stats: rollup.StatsResult{Runs: []rollup.RunStats{{Gaggle: "core", Workflow: "implementation", TotalRuns: 1}}},
		}},
		sources: LocalSources{Definitions: analyticsTestDefinitions(), ReadModel: store},
	}
	got, err := service.TelemetryStats(ctx, TelemetryStatsRequest{Gaggle: "core", Workflow: "implementation"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.CreditAssignment) != 2 {
		t.Fatalf("credit assignment = %+v, want two nodes", got.CreditAssignment)
	}
	var implement, review *NodeCredit
	for i := range got.CreditAssignment {
		switch got.CreditAssignment[i].Stage {
		case "implement":
			implement = &got.CreditAssignment[i]
		case "review":
			review = &got.CreditAssignment[i]
		}
	}
	if implement == nil || implement.Identification != "correlational-fallback" {
		t.Fatalf("implement credit = %+v, want safe correlational fallback", implement)
	}
	if review == nil || review.Identification != "correlational-fallback" {
		t.Fatalf("review credit = %+v, want correlational fallback", review)
	}
	if len(got.PromotionSignals) != 2 || len(got.PromotionCandidates) != 0 {
		t.Fatalf("promotion signals/candidates = %+v / %+v", got.PromotionSignals, got.PromotionCandidates)
	}
	if got.GraphAnalytics == nil || got.GraphAnalytics.Confidence != "untrusted" {
		t.Fatalf("graph analytics = %+v, want untrusted confidence", got.GraphAnalytics)
	}
	for _, signal := range got.PromotionSignals {
		if signal.Source != "correlational-fallback" || signal.PromotionEligible {
			t.Fatalf("fallback signal = %+v", signal)
		}
		if signal.Node == "stage:implement" && signal.Value != 1 {
			t.Fatalf("implement fallback value = %v, want correlational failure share 1", signal.Value)
		}
	}
}

func TestLocalTelemetryStatsProjectsAnalyticsWithoutTerminalEdges(t *testing.T) {
	ctx := context.Background()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), readmodel.FileName))
	if err != nil {
		t.Fatalf("open read model: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 8, 22, 5, 0, 0, 0, time.UTC)
	seedLocalTelemetryRun(t, store, "terminal-edge", "sha256:v1", start, true)

	definitions := &instance.ConfigSet{
		Manifest: &apiv1.Manifest{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				workflow.PreviewFeaturesAnnotation: "true",
			}},
			Spec: apiv1.ManifestSpec{
				Instance: apiv1.InstanceRef{Name: "clubhouse", Environment: apiv1.EnvironmentDev},
			},
		},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
			Spec: apiv1.WorkflowSpec{
				Gaggle: "core", Start: "implement",
				Tasks: []apiv1.Task{
					{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement", Next: "review", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
					{Name: "finish", Type: apiv1.TaskDeterministic, Goal: "finish", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
				},
				Gates: []apiv1.Gate{{
					Name: "review", Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: "status-equals"},
					Branches:  map[string]string{"pass": "finish", "fail": workflow.TargetAbort},
				}},
			},
		}},
	}
	service, err := NewLocal(LocalSources{
		Definitions: definitions, ReadModel: store,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("new local service: %v", err)
	}
	service.telemetry = &Telemetry{store: &fakeTelemetryStore{}}

	got, err := service.TelemetryStats(ctx, TelemetryStatsRequest{Gaggle: "core", Workflow: "implementation"})
	if err != nil {
		t.Fatalf("telemetry stats: %v", err)
	}
	if got.GraphAnalytics == nil {
		t.Fatal("graph analytics is nil")
	}
	if got.GraphAnalytics.Confidence != "untrusted" {
		t.Fatalf("graph analytics confidence = %q, want untrusted", got.GraphAnalytics.Confidence)
	}
	if len(got.GraphAnalytics.CriticalPath.Nodes) != 0 || got.GraphAnalytics.CriticalPath.Weight != 0 {
		t.Fatalf("untrusted critical path = %+v, want withheld", got.GraphAnalytics.CriticalPath)
	}
	for _, score := range got.GraphAnalytics.Centrality {
		if score.Node == workflow.TargetAbort || score.Node == workflow.TargetEscalate {
			t.Fatalf("terminal target appeared in centrality: %+v", got.GraphAnalytics.Centrality)
		}
	}
}

func TestLocalTelemetryStatsClassifiesBoundedAndPartialAnalytics(t *testing.T) {
	for _, test := range []struct {
		name       string
		eligible   []string
		confidence string
	}{
		{name: "bounded", eligible: []string{"implement", "review"}, confidence: "bounded"},
		{name: "partial", eligible: []string{"implement"}, confidence: "partial"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := readmodel.Open(filepath.Join(t.TempDir(), readmodel.FileName))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })

			eligible := make(map[string]bool, len(test.eligible))
			for _, node := range test.eligible {
				eligible[node] = true
			}
			reader := &analyticsReadModel{
				Reader: store,
				credits: []readmodel.NodeCredit{
					{Gaggle: "core", Workflow: "implementation", Kind: "stage", Stage: "implement"},
					{Gaggle: "core", Workflow: "implementation", Kind: "stage", Stage: "review"},
				},
			}
			for _, node := range []string{"implement", "review"} {
				if eligible[node] {
					reader.causal = append(reader.causal, readmodel.CausalNodeCredit{
						Node: "stage:" + node, Effect: 0.5, Lower: 0.1, Upper: 0.9,
						Identification:    readmodel.CausalDifferenceInDifferences,
						IntervalAvailable: true, PromotionEligible: true,
						PromotionSource: string(readmodel.CausalDifferenceInDifferences),
					})
				}
			}
			service, err := NewLocal(LocalSources{
				Layout: instance.NewLayout(t.TempDir()), Definitions: analyticsTestDefinitions(),
				ReadModel: reader,
			}, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			service.telemetry = &Telemetry{store: &fakeTelemetryStore{}}

			got, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{
				Gaggle: "core", Workflow: "implementation",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.GraphAnalytics == nil || got.GraphAnalytics.Confidence != test.confidence {
				t.Fatalf("graph analytics = %+v, want %s confidence", got.GraphAnalytics, test.confidence)
			}
		})
	}
}

func TestLocalTelemetryStatsInfersCrossRunCycle(t *testing.T) {
	ctx := context.Background()
	layout := instance.NewLayout(t.TempDir())
	definitions := analyticsTestDefinitions()
	machine := fixtureMachine(t)
	for i, started := range []time.Time{
		time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC),
	} {
		run, clock := createFixtureRun(t, layout, machine, "cycle-"+string(rune('a'+i)),
			"implementation", "core", started,
			journal.Trigger{Kind: journal.TriggerItem, Ref: "2831"}, true)
		clock.advance(time.Second)
		if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		clock.advance(time.Second)
		if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
			t.Fatal(err)
		}
		clock.advance(time.Second)
		if err := run.Append(journal.Event{Type: journal.EventGateStarted, Gate: "review"}); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			clock.advance(time.Second)
			if err := run.Append(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetAbort}); err != nil {
				t.Fatal(err)
			}
		}
		clock.advance(time.Second)
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}

	store, err := readmodel.Open(filepath.Join(t.TempDir(), readmodel.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reader := &analyticsReadModel{Reader: store}
	service, err := NewLocal(LocalSources{
		Layout: layout, Definitions: definitions, ReadModel: reader,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	service.telemetry = &Telemetry{store: &fakeTelemetryStore{}}

	got, err := service.TelemetryStats(ctx, TelemetryStatsRequest{Gaggle: "core", Workflow: "implementation"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GraphAnalytics == nil || len(got.GraphAnalytics.Cycles) != 1 {
		t.Fatalf("graph analytics = %+v, want one inferred cycle", got.GraphAnalytics)
	}
	cycle := got.GraphAnalytics.Cycles[0]
	if len(cycle) != 2 || cycle[0] != "implement" || cycle[1] != "review" {
		t.Fatalf("inferred cycle = %v, want [implement review]", cycle)
	}
}

func TestNormalizedPromotionFailureProducesTrustedWeightsForGraphAnalytics(t *testing.T) {
	credits := []NodeCredit{
		{Kind: "stage", Stage: "implement", Identity: "sha256:v1"},
		{Kind: "stage", Stage: "review", Identity: "sha256:v1"},
	}
	signals := []PromotionSignal{
		{Node: "stage:implement", Value: 0.8, PromotionEligible: true},
	}

	failure, trusted, nodes := normalizedPromotionFailure(credits, signals)
	if len(nodes) != 2 || len(trusted) != 1 {
		t.Fatalf("graph confidence node sets = %v / %v, want all credits and one trusted node", nodes, trusted)
	}
	if failure["implement"] != 0.8 || failure["review"] != 0 {
		t.Fatalf("graph failure weights = %v, want implement 0.8 and review 0", failure)
	}
}

func TestEligiblePromotionSignalsExcludesFallbacks(t *testing.T) {
	signals := []PromotionSignal{
		{
			Node: "stage:implement", Value: 0.6, Source: "correlational-fallback",
			Caveat: "observational", PromotionEligible: false,
		},
		{
			Node: "stage:review", Value: -0.2, Lower: -0.3, Upper: -0.1,
			Source: string(readmodel.CausalRandomized), Caveat: "randomized",
			PromotionEligible: true,
		},
	}
	got := EligiblePromotionSignals(signals)
	if len(got) != 1 || got[0].Node != "stage:review" {
		t.Fatalf("eligible promotion signals = %+v", got)
	}
}

func TestNormalizedPromotionFailureAggregatesIdentityCredits(t *testing.T) {
	credits := []NodeCredit{
		{Kind: "stage", Stage: "review", Identity: "sha256:a"},
		{Kind: "stage", Stage: "review", Identity: "sha256:b"},
		{Kind: "stage", Stage: "implement", Identity: "sha256:c"},
	}
	signals := []PromotionSignal{
		{Node: "stage:review", Value: 0.4, PromotionEligible: true},
		{Node: "stage:review", Value: 0.8, PromotionEligible: true},
	}

	failure, trusted, nodes := normalizedPromotionFailure(credits, signals)
	if len(nodes) != 2 || len(trusted) != 1 {
		t.Fatalf("normalized node sets = %v / %v, want two credit nodes and one trusted node", nodes, trusted)
	}
	if math.Abs(failure["review"]-0.6) > 1e-12 {
		t.Fatalf("review failure = %v, want aggregated mean 0.6", failure["review"])
	}
	if failure["implement"] != 0 {
		t.Fatalf("implement failure = %v, want zero without eligible signal", failure["implement"])
	}
}

func TestNormalizedPromotionFailureUsesUniqueNodesForBoundedConfidence(t *testing.T) {
	credits := []NodeCredit{
		{Kind: "stage", Stage: "review", Identity: "sha256:a"},
		{Kind: "stage", Stage: "review", Identity: "sha256:b"},
	}
	signals := []PromotionSignal{
		{Node: "stage:review", Value: 0.4, PromotionEligible: true},
		{Node: "stage:review", Value: 0.8, PromotionEligible: true},
	}

	_, trusted, nodes := normalizedPromotionFailure(credits, signals)
	if len(trusted) != len(nodes) {
		t.Fatalf("trusted and credit node counts = %d / %d, want equal unique counts", len(trusted), len(nodes))
	}
}

func TestNormalizedPromotionFailureRequiresMatchingNodeIdentities(t *testing.T) {
	credits := []NodeCredit{
		{Kind: "stage", Stage: "implement"},
		{Kind: "stage", Stage: "review"},
	}
	signals := []PromotionSignal{
		{Node: "stage:implement", Value: 0.4, PromotionEligible: true},
		{Node: "stage:other", Value: 0.8, PromotionEligible: true},
	}

	_, trusted, nodes := normalizedPromotionFailure(credits, signals)
	if sameAnalyticsNodes(trusted, nodes) {
		t.Fatalf("trusted nodes = %v, credit nodes = %v, want identity mismatch", trusted, nodes)
	}
}

func TestTelemetryStatsSerializesGraphAnalytics(t *testing.T) {
	result := TelemetryStatsResult{
		GraphAnalytics: &readmodel.GraphAnalytics{
			Centrality: []readmodel.CentralityScore{{Node: "review", Score: 1.5}},
			CriticalPath: readmodel.CriticalPath{
				Nodes: []string{"implement", "review"}, Weight: 42,
			},
			Cycles:     [][]string{{"review", "ci"}},
			Confidence: "partial",
			Caveat:     "fallback excluded",
		},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TelemetryStatsResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.GraphAnalytics, result.GraphAnalytics) {
		t.Fatalf("graph analytics = %+v, want %+v", decoded.GraphAnalytics, result.GraphAnalytics)
	}
}

func seedLocalTelemetryRun(t *testing.T, store *readmodel.Store, suffix, identity string, startedAt time.Time, failed bool) {
	t.Helper()
	phase := journal.PhaseCompleted
	verdict, target := "pass", ""
	if failed {
		phase = journal.PhaseFailed
		verdict, target = "fail", "@abort"
	}
	runID := "causal-" + suffix + "-" + startedAt.Format("150405.000000000")
	finishedAt := startedAt.Add(time.Minute)
	err := store.UpsertRun(context.Background(), readmodel.Projection{
		Run: readmodel.RunRow{
			RunID: runID, Gaggle: "core", Workflow: "implementation",
			WorkflowVersion: 1, WorkflowDigest: "sha256:wf",
			Phase: phase, Terminal: true, StartedAt: startedAt, FinishedAt: &finishedAt,
			LastActivity: finishedAt, LastSeq: 1, OutcomeVerdict: verdict, OutcomeTarget: target,
		},
		Nodes: []readmodel.NodeRow{
			{RunID: runID, Kind: "stage", Name: "implement", Identity: identity, Attempts: 1},
		},
	})
	if err != nil {
		t.Fatalf("seed run %s: %v", runID, err)
	}
}

func analyticsTestDefinitions() *instance.ConfigSet {
	return &instance.ConfigSet{
		Manifest: &apiv1.Manifest{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				workflow.PreviewFeaturesAnnotation: "true",
			}},
			Spec: apiv1.ManifestSpec{
				Instance: apiv1.InstanceRef{Name: "clubhouse", Environment: apiv1.EnvironmentDev},
			},
		},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
			Spec: apiv1.WorkflowSpec{
				Gaggle: "core",
				Start:  "implement",
				Tasks: []apiv1.Task{
					{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement", Next: "review", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
					{Name: "finish", Type: apiv1.TaskDeterministic, Goal: "finish", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
				},
				Gates: []apiv1.Gate{{
					Name: "review", Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: "status-equals"},
					Branches:  map[string]string{"pass": "finish", "fail": workflow.TargetAbort},
				}},
			},
		}},
	}
}

func seedLocalTelemetryOtherNode(t *testing.T, store *readmodel.Store, startedAt time.Time) {
	t.Helper()
	finishedAt := startedAt.Add(time.Minute)
	runID := "fallback-review"
	if err := store.UpsertRun(context.Background(), readmodel.Projection{
		Run: readmodel.RunRow{
			RunID: runID, Gaggle: "core", Workflow: "implementation",
			WorkflowVersion: 1, WorkflowDigest: "sha256:wf",
			Phase: journal.PhaseFailed, Terminal: true, StartedAt: startedAt, FinishedAt: &finishedAt,
			LastActivity: finishedAt, LastSeq: 1, OutcomeVerdict: "fail", OutcomeTarget: "@abort",
		},
		Nodes: []readmodel.NodeRow{
			{RunID: runID, Kind: "stage", Name: "review", Identity: "sha256:review", Attempts: 1},
		},
	}); err != nil {
		t.Fatalf("seed fallback run: %v", err)
	}
}
