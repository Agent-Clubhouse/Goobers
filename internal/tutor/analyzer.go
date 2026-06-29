package tutor

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const maxEvidence = 5

// Thresholds controls when telemetry becomes actionable.
type Thresholds struct {
	MinGateEvaluations     int
	GateRejectionRate      float64
	MinTaskExecutions      int
	TaskFailureRate        float64
	SlowTaskDuration       time.Duration
	MinSlowTaskOccurrences int
	RetryCount             int
	MinRetryOccurrences    int
}

// DefaultThresholds returns conservative defaults that require repeated signals.
func DefaultThresholds() Thresholds {
	return Thresholds{
		MinGateEvaluations:     3,
		GateRejectionRate:      0.50,
		MinTaskExecutions:      3,
		TaskFailureRate:        0.50,
		SlowTaskDuration:       10 * time.Minute,
		MinSlowTaskOccurrences: 3,
		RetryCount:             2,
		MinRetryOccurrences:    3,
	}
}

// Analyzer turns telemetry samples into deterministic improvement findings.
type Analyzer struct {
	Thresholds Thresholds
}

// NewAnalyzer constructs an Analyzer with default thresholds when unset.
func NewAnalyzer(thresholds Thresholds) Analyzer {
	if thresholds.MinGateEvaluations == 0 &&
		thresholds.MinTaskExecutions == 0 &&
		thresholds.SlowTaskDuration == 0 &&
		thresholds.RetryCount == 0 {
		thresholds = DefaultThresholds()
	}
	return Analyzer{Thresholds: thresholds.withDefaults()}
}

// Analyze returns sorted findings for repeated gate, task, latency, or retry signals.
func (a Analyzer) Analyze(signals []Signal) []Finding {
	thresholds := a.Thresholds.withDefaults()
	gates := map[string]*aggregate{}
	tasks := map[string]*aggregate{}
	slowTasks := map[string]*aggregate{}
	retries := map[string]*aggregate{}

	for _, sig := range signals {
		switch sig.Kind {
		case SignalGate:
			key := aggregateKey(sig, sig.GateID)
			agg := aggregateFor(gates, key, sig)
			agg.observed++
			if gateRejected(sig) {
				agg.problem++
				agg.addEvidence(sig)
			}
		case SignalTask:
			key := aggregateKey(sig, sig.TaskID)
			agg := aggregateFor(tasks, key, sig)
			agg.observed++
			if taskFailed(sig) {
				agg.problem++
				agg.addEvidence(sig)
			}
			if sig.Duration >= thresholds.SlowTaskDuration && thresholds.SlowTaskDuration > 0 {
				slow := aggregateFor(slowTasks, key, sig)
				slow.observed++
				slow.problem++
				slow.addEvidence(sig)
			}
			if sig.RetryCount >= thresholds.RetryCount && thresholds.RetryCount > 0 {
				retry := aggregateFor(retries, key, sig)
				retry.observed++
				retry.problem++
				retry.addEvidence(sig)
			}
		}
	}

	findings := make([]Finding, 0)
	for _, agg := range gates {
		rate := ratio(agg.problem, agg.observed)
		if agg.observed >= thresholds.MinGateEvaluations && rate >= thresholds.GateRejectionRate {
			findings = append(findings, agg.finding(FindingGateRejection, rate))
		}
	}
	for _, agg := range tasks {
		rate := ratio(agg.problem, agg.observed)
		if agg.observed >= thresholds.MinTaskExecutions && rate >= thresholds.TaskFailureRate {
			findings = append(findings, agg.finding(FindingTaskFailure, rate))
		}
	}
	for _, agg := range slowTasks {
		if agg.problem >= thresholds.MinSlowTaskOccurrences {
			findings = append(findings, agg.finding(FindingSlowTask, 1))
		}
	}
	for _, agg := range retries {
		if agg.problem >= thresholds.MinRetryOccurrences {
			findings = append(findings, agg.finding(FindingRetries, 1))
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return severityRank(findings[i].Severity) > severityRank(findings[j].Severity)
		}
		if findings[i].Rate != findings[j].Rate {
			return findings[i].Rate > findings[j].Rate
		}
		return findingSortKey(findings[i]) < findingSortKey(findings[j])
	})
	return findings
}

func (t Thresholds) withDefaults() Thresholds {
	defaults := DefaultThresholds()
	if t.MinGateEvaluations == 0 {
		t.MinGateEvaluations = defaults.MinGateEvaluations
	}
	if t.GateRejectionRate == 0 {
		t.GateRejectionRate = defaults.GateRejectionRate
	}
	if t.MinTaskExecutions == 0 {
		t.MinTaskExecutions = defaults.MinTaskExecutions
	}
	if t.TaskFailureRate == 0 {
		t.TaskFailureRate = defaults.TaskFailureRate
	}
	if t.SlowTaskDuration == 0 {
		t.SlowTaskDuration = defaults.SlowTaskDuration
	}
	if t.MinSlowTaskOccurrences == 0 {
		t.MinSlowTaskOccurrences = defaults.MinSlowTaskOccurrences
	}
	if t.RetryCount == 0 {
		t.RetryCount = defaults.RetryCount
	}
	if t.MinRetryOccurrences == 0 {
		t.MinRetryOccurrences = defaults.MinRetryOccurrences
	}
	return t
}

type aggregate struct {
	gaggle     string
	workflowID string
	taskID     string
	gateID     string
	gooberID   string
	observed   int
	problem    int
	evidence   []Evidence
}

func aggregateFor(items map[string]*aggregate, key string, sig Signal) *aggregate {
	if agg, ok := items[key]; ok {
		if agg.gooberID == "" {
			agg.gooberID = sig.GooberID
		}
		return agg
	}
	agg := &aggregate{
		gaggle:     sig.Gaggle,
		workflowID: sig.WorkflowID,
		taskID:     sig.TaskID,
		gateID:     sig.GateID,
		gooberID:   sig.GooberID,
	}
	items[key] = agg
	return agg
}

func (a *aggregate) addEvidence(sig Signal) {
	if len(a.evidence) >= maxEvidence {
		return
	}
	a.evidence = append(a.evidence, evidenceFor(sig))
}

func (a *aggregate) finding(kind FindingType, rate float64) Finding {
	f := Finding{
		Type:         kind,
		Severity:     severity(rate, a.problem),
		Gaggle:       a.gaggle,
		WorkflowID:   a.workflowID,
		TaskID:       a.taskID,
		GateID:       a.gateID,
		GooberID:     a.gooberID,
		Observed:     a.observed,
		ProblemCount: a.problem,
		Rate:         rate,
		Evidence:     append([]Evidence(nil), a.evidence...),
	}
	f.Rationale, f.Recommendation = findingText(f)
	return f
}

func aggregateKey(sig Signal, stateID string) string {
	return strings.Join([]string{sig.Gaggle, sig.WorkflowID, stateID}, "\x00")
}

func gateRejected(sig Signal) bool {
	decision := strings.ToLower(strings.TrimSpace(sig.Decision))
	if decision == "" {
		return taskFailed(sig)
	}
	switch decision {
	case "pass", "passed", "approve", "approved", "success", "succeeded", "ok":
		return false
	default:
		return true
	}
}

func taskFailed(sig Signal) bool {
	return strings.EqualFold(sig.Status, statusError) || sig.Error != ""
}

func ratio(problem, observed int) float64 {
	if observed == 0 {
		return 0
	}
	return float64(problem) / float64(observed)
}

func severity(rate float64, count int) string {
	if count >= 5 || rate >= 0.75 {
		return "high"
	}
	if count >= 3 || rate >= 0.5 {
		return "medium"
	}
	return "low"
}

func severityRank(value string) int {
	switch value {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func findingSortKey(f Finding) string {
	return strings.Join([]string{string(f.Type), f.Gaggle, f.WorkflowID, f.TaskID, f.GateID}, "/")
}

func findingText(f Finding) (string, string) {
	target := f.WorkflowID
	if f.TaskID != "" {
		target += "/" + f.TaskID
	}
	if f.GateID != "" {
		target += "/" + f.GateID
	}
	switch f.Type {
	case FindingGateRejection:
		return fmt.Sprintf("Gate %q in workflow %q rejected %d of %d recent evaluations (%.0f%%).", f.GateID, f.WorkflowID, f.ProblemCount, f.Observed, f.Rate*100),
			"Strengthen the workflow or gate definition with clearer required evidence, a preflight check, or more specific reviewer instructions before the gate runs."
	case FindingTaskFailure:
		return fmt.Sprintf("Task %q in workflow %q failed %d of %d recent executions (%.0f%%).", f.TaskID, f.WorkflowID, f.ProblemCount, f.Observed, f.Rate*100),
			"Update the responsible goober instructions or workflow task definition with failure-specific guidance and expected diagnostics."
	case FindingSlowTask:
		return fmt.Sprintf("Task %q in workflow %q exceeded the slow-task threshold %d times.", f.TaskID, f.WorkflowID, f.ProblemCount),
			"Add workflow guidance that narrows scope, encourages earlier deterministic checks, or splits the task into smaller states."
	case FindingRetries:
		return fmt.Sprintf("Task %q in workflow %q needed retry-heavy execution %d times.", f.TaskID, f.WorkflowID, f.ProblemCount),
			"Clarify retry-prone instructions and add deterministic preconditions so repeated attempts have actionable feedback."
	default:
		return fmt.Sprintf("Telemetry surfaced repeated issues for %s.", target), "Review and improve the relevant config definition."
	}
}
