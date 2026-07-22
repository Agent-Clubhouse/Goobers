package boundedwait

import (
	"testing"
	"time"
)

func TestPollBudgets(t *testing.T) {
	tests := []struct {
		name   string
		stage  time.Duration
		budget func(time.Duration) time.Duration
		want   time.Duration
	}{
		{name: "ci normal margin", stage: 12 * time.Second, budget: CIPollBudget, want: 11 * time.Second},
		{name: "ci short stage", stage: 500 * time.Millisecond, budget: CIPollBudget, want: 450 * time.Millisecond},
		{name: "merge queue proportional margin", stage: 30 * time.Minute, budget: MergeQueuePollBudget, want: 27 * time.Minute},
		{name: "merge queue minimum margin", stage: 90 * time.Second, budget: MergeQueuePollBudget, want: 30 * time.Second},
		{name: "merge queue degenerate stage", stage: 30 * time.Second, budget: MergeQueuePollBudget, want: 15 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.budget(tc.stage); got != tc.want {
				t.Fatalf("budget(%s) = %s, want %s", tc.stage, got, tc.want)
			}
		})
	}
}
