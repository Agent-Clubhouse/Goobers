package workflowcluster

import (
	"math"
	"sort"

	"github.com/goobers/goobers/internal/journal"
)

// DefaultFitnessMinRuns is the smallest sample reported as sufficient.
const DefaultFitnessMinRuns = 5

// WilsonInterval is a 95% Wilson score interval for a proportion.
type WilsonInterval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// ClusterFitness summarizes retained terminal runs for one structural cluster.
// It is advisory and is never used to order, promote, or gate clusters.
type ClusterFitness struct {
	ClusterID             string         `json:"clusterId"`
	RunCount              int            `json:"runCount"`
	MinimumRuns           int            `json:"minimumRuns"`
	InsufficientRuns      bool           `json:"insufficientRuns"`
	SuccessRate           float64        `json:"successRate"`
	SuccessInterval       WilsonInterval `json:"successInterval"`
	MedianWallTimeSeconds float64        `json:"medianWallTimeSeconds"`
	P90WallTimeSeconds    float64        `json:"p90WallTimeSeconds"`
	MedianCostUSD         float64        `json:"medianCostUsd"`
	P90CostUSD            float64        `json:"p90CostUsd"`
	HumanGateRate         float64        `json:"humanGateRate"`
	EscalationRate        float64        `json:"escalationRate"`
	RetryLoopRate         float64        `json:"retryLoopRate"`
}

// FitnessReport is the run scan and per-cluster fitness shown by the CLI and
// Labs. Clusters always have an entry, including when none of their members
// have retained runs.
type FitnessReport struct {
	Scan     BehaviorScan     `json:"scan"`
	Clusters []ClusterFitness `json:"clusters"`
}

// EvaluateFitness joins retained behavior descriptors to current structural
// cluster members. Both workflow identity and digest must match, so runs from
// an older definition are not attributed to the current shape.
func EvaluateFitness(model Model, workflowDigests map[string]string, scan BehaviorScan, minRuns int) FitnessReport {
	if minRuns <= 0 {
		minRuns = DefaultFitnessMinRuns
	}
	byWorkflow := make(map[string][]BehaviorDescriptor)
	for _, d := range scan.Descriptors {
		id := d.Gaggle + "/" + d.Workflow
		if digest := workflowDigests[id]; digest != "" && digest == d.WorkflowDigest {
			byWorkflow[id] = append(byWorkflow[id], d)
		}
	}
	report := FitnessReport{Scan: scan, Clusters: make([]ClusterFitness, 0, len(model.Clusters))}
	report.Scan.Descriptors = nil
	for _, cluster := range model.Clusters {
		var runs []BehaviorDescriptor
		for _, member := range cluster.Members {
			runs = append(runs, byWorkflow[member]...)
		}
		report.Clusters = append(report.Clusters, summarizeFitness(cluster.ID, runs, minRuns))
	}
	return report
}

func summarizeFitness(clusterID string, runs []BehaviorDescriptor, minRuns int) ClusterFitness {
	f := ClusterFitness{
		ClusterID: clusterID, RunCount: len(runs), MinimumRuns: minRuns,
		InsufficientRuns: len(runs) < minRuns,
	}
	if len(runs) == 0 {
		return f
	}
	wall := make([]float64, len(runs))
	cost := make([]float64, len(runs))
	var successes, human, escalated, retryLoop int
	for i, run := range runs {
		wall[i] = math.Expm1(run.Numeric["duration.log1pSeconds"])
		cost[i] = math.Expm1(run.Numeric["usage.log1pCostCents"]) / 100
		if run.Outcome == journal.PhaseCompleted {
			successes++
		}
		if run.Numeric["gates.humanPaused"] > 0 {
			human++
		}
		if run.Outcome == journal.PhaseEscalated || run.Numeric["gates.escalated"] > 0 {
			escalated++
		}
		if run.Numeric["visits.retry"] > 0 || run.Numeric["visits.loop"] > 0 ||
			run.Numeric["retries.policy"] > 0 || run.Numeric["retries.infra"] > 0 ||
			run.Numeric["retries.human"] > 0 || run.Numeric["reruns"] > 0 {
			retryLoop++
		}
	}
	sort.Float64s(wall)
	sort.Float64s(cost)
	n := float64(len(runs))
	f.SuccessRate = float64(successes) / n
	f.SuccessInterval = wilson95(successes, len(runs))
	f.MedianWallTimeSeconds, f.P90WallTimeSeconds = median(wall), nearestRank(wall, 0.9)
	f.MedianCostUSD, f.P90CostUSD = median(cost), nearestRank(cost, 0.9)
	f.HumanGateRate = float64(human) / n
	f.EscalationRate = float64(escalated) / n
	f.RetryLoopRate = float64(retryLoop) / n
	return f
}

func median(sorted []float64) float64 {
	middle := len(sorted) / 2
	if len(sorted)%2 != 0 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func wilson95(successes, total int) WilsonInterval {
	if total == 0 {
		return WilsonInterval{}
	}
	const z = 1.959963984540054
	n := float64(total)
	p := float64(successes) / n
	denominator := 1 + z*z/n
	center := (p + z*z/(2*n)) / denominator
	margin := z * math.Sqrt((p*(1-p)+z*z/(4*n))/n) / denominator
	return WilsonInterval{Lower: center - margin, Upper: center + margin}
}
