package readservice

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestTelemetryCostsProjectsBoundedAggregateContract(t *testing.T) {
	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(7 * 24 * time.Hour)
	nanoAIU := int64(2_500_000_000)
	costUSD := 0.025
	store := &fakeTelemetryStore{costs: rollup.CostResult{
		PullRequests: []rollup.CostAggregate{{
			Provider: "github", ExternalKind: "pr", ExternalID: "4398",
			TotalRuns: 3, MeasuredRuns: 2, TotalAttempts: 4, MeasuredAttempts: 3,
			NanoAIU: &nanoAIU, CostUSD: &costUSD,
			BillingModels: []string{telemetry.BillingModelAICredits},
			CostBases:     []string{telemetry.CostBasisVendorReported},
			Models: []rollup.CostModelAggregate{{
				Model: "gpt-5.6-sol", UsageAttempts: 3, MeasuredAttempts: 3,
				NanoAIU: &nanoAIU, CostUSD: &costUSD,
				BillingModels: []string{telemetry.BillingModelAICredits},
				CostBases:     []string{telemetry.CostBasisVendorReported},
			}},
		}},
	}}
	service := &Telemetry{store: store}

	got, err := service.TelemetryCosts(context.Background(), TelemetryCostRequest{
		Provider: "github", Scope: TelemetryCostScopePullRequest, ExternalID: "4398",
		Since: since, Until: until,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantQuery := rollup.CostQuery{
		Provider: "github", ExternalKind: rollup.CostExternalKindPR, ExternalID: "4398",
		Since: since, Until: until,
	}
	if !reflect.DeepEqual(store.costReq, wantQuery) {
		t.Fatalf("store query = %+v, want %+v", store.costReq, wantQuery)
	}
	if len(got.PullRequests) != 1 || len(got.Issues) != 0 {
		t.Fatalf("result = %+v", got)
	}
	item := got.PullRequests[0]
	if item.Coverage.Complete || !item.Coverage.LowerBound ||
		item.Coverage.MeasuredRuns != 2 || item.Coverage.TotalRuns != 3 {
		t.Fatalf("coverage = %+v", item.Coverage)
	}
	if len(item.NativeTotals) != 1 || item.NativeTotals[0].Unit != "aiCredits" ||
		item.NativeTotals[0].Value != 2.5 || item.NativeTotals[0].Estimated {
		t.Fatalf("native totals = %+v", item.NativeTotals)
	}
	if len(item.NormalizedTotals) != 2 ||
		item.NormalizedTotals[1].Unit != "usd" ||
		item.NormalizedTotals[1].Value != 0.025 ||
		!item.NormalizedTotals[1].Estimated {
		t.Fatalf("normalized totals = %+v", item.NormalizedTotals)
	}
	if len(item.Models) != 1 || item.Models[0].Model != "gpt-5.6-sol" {
		t.Fatalf("models = %+v", item.Models)
	}
}

func TestTelemetryCostsValidatesBeforeStoreAndHonorsCancellation(t *testing.T) {
	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	tests := []TelemetryCostRequest{
		{Scope: "unknown", Since: since, Until: since.Add(time.Hour)},
		{Scope: TelemetryCostScopePullRequest, Since: since, Until: since.Add(time.Hour)},
		{Scope: TelemetryCostScopeSummary, ExternalID: "1", Since: since, Until: since.Add(time.Hour)},
		{Scope: TelemetryCostScopeSummary, Since: since, Until: since},
		{Scope: TelemetryCostScopeSummary, Since: since, Until: since.Add(MaxTelemetryCostWindow + time.Second)},
	}
	for _, req := range tests {
		store := &fakeTelemetryStore{}
		_, err := (&Telemetry{store: store}).TelemetryCosts(context.Background(), req)
		if !errors.Is(err, ErrInvalidTelemetryRequest) {
			t.Fatalf("TelemetryCosts(%+v) error = %v", req, err)
		}
		if store.costCalls != 0 {
			t.Fatalf("TelemetryCosts(%+v) queried the store", req)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &fakeTelemetryStore{}
	_, err := (&Telemetry{store: store}).TelemetryCosts(ctx, TelemetryCostRequest{
		Scope: TelemetryCostScopeSummary, Since: since, Until: since.Add(time.Hour),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled TelemetryCosts error = %v", err)
	}
	if store.costCalls != 0 {
		t.Fatal("cancelled request queried the store")
	}
}
