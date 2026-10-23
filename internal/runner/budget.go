package runner

import (
	"fmt"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
)

type attemptUsageCollector struct {
	mu       sync.Mutex
	metrics  map[string]float64
	reported bool
}

func (c *attemptUsageCollector) report(metrics map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reported = true
	if c.metrics == nil {
		c.metrics = make(map[string]float64)
	}
	for name, value := range metrics {
		if telemetry.IsCanonicalAgentUsageMetric(name) {
			c.metrics[name] = value
		}
	}
}

func (c *attemptUsageCollector) snapshot() (map[string]float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	metrics := make(map[string]float64, len(c.metrics))
	for name, value := range c.metrics {
		metrics[name] = value
	}
	return metrics, c.reported
}

func interruptedStageBudgetFailure(limits apiv1.Limits) apiv1.ResultEnvelope {
	var violations []string
	if limits.MaxTokens > 0 {
		violations = append(violations, fmt.Sprintf(
			"cannot enforce maxTokens %d: interrupted attempt usage is unavailable",
			limits.MaxTokens,
		))
	}
	if limits.MaxCostUSD > 0 {
		violations = append(violations, fmt.Sprintf(
			"cannot enforce maxCostUSD %g: interrupted attempt usage is unavailable",
			limits.MaxCostUSD,
		))
	}
	return runsemantics.BudgetFailure(apiv1.ResultEnvelope{}, violations)
}

func usageBudgetConfigured(limits apiv1.Limits) bool {
	return limits.MaxTokens > 0 || limits.MaxCostUSD > 0
}

// BudgetExceededErrorCode is the shared non-retryable usage failure code.
const BudgetExceededErrorCode = runsemantics.BudgetExceededErrorCode
