package main

import (
	"testing"

	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestFormatMergedPullRequestCost(t *testing.T) {
	nanoAIU := int64(12_400_000_000)
	costUSD := 0.124
	premium := 3.5

	tests := []struct {
		name      string
		aggregate rollup.CostAggregate
		want      string
	}{
		{
			name:      "AI credits and USD",
			aggregate: rollup.CostAggregate{NanoAIU: &nanoAIU, CostUSD: &costUSD},
			want:      "12.40 AI credits (~$0.12)",
		},
		{
			name:      "estimated USD",
			aggregate: rollup.CostAggregate{CostUSD: &costUSD},
			want:      "$0.12 estimated",
		},
		{
			name:      "legacy premium requests",
			aggregate: rollup.CostAggregate{CopilotPremiumRequests: &premium},
			want:      "3.50 premium requests",
		},
		{name: "unmeasured"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatMergedPullRequestCost(tt.aggregate); got != tt.want {
				t.Fatalf("formatMergedPullRequestCost() = %q, want %q", got, tt.want)
			}
		})
	}
}
