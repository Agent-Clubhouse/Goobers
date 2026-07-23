package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry/rollup"
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
	signatureCalls int
}

func (f *fakeTelemetryStore) Stats(req rollup.StatsRequest) (rollup.StatsResult, error) {
	f.statsCalled++
	f.statsReq = req
	return f.stats, f.err
}

func (f *fakeTelemetryStore) TopErrorSignatures(req rollup.StatsRequest, limit int) ([]rollup.ErrorSignature, error) {
	f.signatureCalls++
	f.signatureReq = req
	f.signatureLimit = limit
	return f.signatures, f.err
}

func (f *fakeTelemetryStore) Errors(req rollup.ErrorsRequest) ([]rollup.ErrorEvent, error) {
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
				Gaggle: "core", Workflow: "failed", Stage: "done", Model: "gpt-5.6-sol", HarnessVersion: "copilot version 1.2.3", TotalAttempts: 2, FailedAttempts: 1, HasDuration: true,
				DurationSamples: 2, P50DurationMs: 10, P95DurationMs: 20,
				TokenSamples: 2, P50Tokens: 100, P95Tokens: 200, HasTokens: true,
				CostSamples: 2, P50CostUSD: 0.5, P95CostUSD: 1, HasCost: true,
				RetryWasteAttempts: 1, RetryWasteDurationMs: 10, HasRetryWasteDuration: true,
				RetryWasteTokens: 100, HasRetryWasteTokens: true,
				RetryWasteCostUSD: 0.5, HasRetryWasteCost: true,
			},
			{Gaggle: "core", Workflow: "running", Stage: "active", TotalAttempts: 1},
		},
		Models: []rollup.ModelStats{{
			Model: "gpt-5.4", UsageSamples: 1,
			InputTokenSamples: 1, InputTokens: 0, HasInputTokens: true,
		}},
	}}
	service := &Telemetry{store: store}

	got, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{
		Workflow:              "implement",
		Gaggle:                "core",
		Model:                 "gpt-5.6-sol",
		HarnessVersion:        "copilot version 1.2.3",
		GroupByModel:          true,
		GroupByHarnessVersion: true,
		Since:                 since,
		Until:                 until,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReq := rollup.StatsRequest{
		Workflow: "implement", Gaggle: "core", Model: "gpt-5.6-sol", HarnessVersion: "copilot version 1.2.3",
		GroupByModel: true, GroupByHarnessVersion: true, Since: since, Until: until,
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
	if got.Gaggles == nil || got.Runs == nil || got.Stages == nil || got.Models == nil ||
		len(got.Gaggles) != 0 || len(got.Runs) != 0 || len(got.Stages) != 0 || len(got.Models) != 0 {
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
