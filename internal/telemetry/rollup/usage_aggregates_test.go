package rollup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

type usageAttemptFixture struct {
	number         int
	duration       time.Duration
	status         string
	model          string
	harnessVersion string
	metrics        map[string]float64
	modelUsage     []telemetry.ModelUsage
	skipSpan       bool
}

func seedUsageRun(
	t *testing.T,
	runsDir, runID, workflow, stage string,
	startedAt time.Time,
	attempts ...usageAttemptFixture,
) string {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, filepath.Join(dir, dirSpans))
	mustWriteFile(t, filepath.Join(dir, fileRunYAML),
		strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: "+workflow))

	seq := 1
	cursor := startedAt
	eventLines := []string{eventLine(seq, cursor, `"type":"run.started"`)}
	var spanLines []string
	for i, attempt := range attempts {
		attemptNumber := attempt.number
		if attemptNumber == 0 {
			attemptNumber = i + 1
		}
		seq++
		cursor = cursor.Add(time.Millisecond)
		started := cursor
		eventLines = append(eventLines, eventLine(seq, started,
			fmt.Sprintf(`"type":"stage.started","stage":%q,"attempt":%d,"attemptClass":"policy"`, stage, attemptNumber)))

		cursor = cursor.Add(attempt.duration)
		seq++
		eventLines = append(eventLines, eventLine(seq, cursor,
			fmt.Sprintf(`"type":"stage.finished","stage":%q,"attempt":%d,"status":%q`, stage, attemptNumber, attempt.status)))

		attrs := map[string]string{
			telemetry.AttrStage:         stage,
			telemetry.AttrAttemptNumber: strconv.Itoa(attemptNumber),
		}
		if attempt.model != "" || attempt.harnessVersion != "" {
			attrs[telemetry.AttrModel] = attempt.model
			attrs[telemetry.AttrHarnessVersion] = attempt.harnessVersion
		}
		for name, value := range attempt.metrics {
			switch name {
			case telemetry.AttrGenAIUsageInputTokens, telemetry.AttrGenAIUsageOutputTokens:
				attrs[name] = strconv.FormatInt(int64(value), 10)
			default:
				attrs[name] = strconv.FormatFloat(value, 'f', -1, 64)
			}
		}
		if attempt.skipSpan {
			continue
		}
		record := telemetry.SpanRecord{
			Schema:     telemetry.SpanSchema,
			TraceID:    runID,
			SpanID:     fmt.Sprintf("%016x", i+1),
			Name:       "task/" + stage,
			Kind:       telemetry.SpanKindTask,
			StartTime:  started,
			EndTime:    cursor,
			Status:     "ok",
			Attributes: attrs,
		}
		for _, usage := range attempt.modelUsage {
			eventAttrs := map[string]string{telemetry.AttrGenAIResponseModel: usage.Model}
			if usage.InputTokens != nil {
				eventAttrs[telemetry.AttrGenAIUsageInputTokens] = strconv.FormatInt(*usage.InputTokens, 10)
			}
			if usage.OutputTokens != nil {
				eventAttrs[telemetry.AttrGenAIUsageOutputTokens] = strconv.FormatInt(*usage.OutputTokens, 10)
			}
			if usage.CopilotPremiumRequests != nil {
				eventAttrs[telemetry.AttrCopilotPremiumRequests] = strconv.FormatFloat(*usage.CopilotPremiumRequests, 'f', -1, 64)
			}
			if usage.CostUSD != nil {
				eventAttrs[telemetry.AttrUsageCostUSD] = strconv.FormatFloat(*usage.CostUSD, 'f', -1, 64)
			}
			record.Events = append(record.Events, telemetry.SpanEventRecord{
				Name:       telemetry.GenAIModelUsageEventName,
				Time:       cursor,
				Attributes: eventAttrs,
			})
		}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal span fixture: %v", err)
		}
		spanLines = append(spanLines, string(data))
	}
	seq++
	cursor = cursor.Add(time.Millisecond)
	eventLines = append(eventLines, eventLine(seq, cursor, `"type":"run.finished","status":"completed"`))
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(eventLines, "\n")+"\n")
	mustWriteFile(t, filepath.Join(dir, dirSpans, fileSpans), strings.Join(spanLines, "\n")+"\n")
	return dir
}

func int64Usage(value int64) *int64       { return &value }
func float64Usage(value float64) *float64 { return &value }

func TestUsageRollupPreservesTaskRepasses(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dir := seedUsageRun(t, runsDir, fixtureRunID, "implement", "agent", fixtureStart,
		usageAttemptFixture{number: 1, duration: 10 * time.Millisecond, status: "failure", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 5, telemetry.AttrGenAIUsageOutputTokens: 10,
			telemetry.AttrUsageCostUSD: 0.5,
		}},
		usageAttemptFixture{number: 1, duration: 20 * time.Millisecond, status: "success", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 15, telemetry.AttrGenAIUsageOutputTokens: 20,
			telemetry.AttrUsageCostUSD: 1.5,
		}})

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun task repass: %v", err)
	}

	attempts, err := db.StageAttempts(fixtureRunID)
	if err != nil {
		t.Fatalf("StageAttempts: %v", err)
	}
	if len(attempts) != 2 ||
		attempts[0].Traversal != 1 || attempts[0].Attempt != 1 ||
		attempts[1].Traversal != 2 || attempts[1].Attempt != 1 {
		t.Fatalf("repass attempts = %#v, want traversals 1/2 with local attempt 1", attempts)
	}
	if attempts[0].InputTokens == nil || *attempts[0].InputTokens != 5 ||
		attempts[1].InputTokens == nil || *attempts[1].InputTokens != 15 {
		t.Fatalf("repass usage = %#v, want usage attached to each traversal", attempts)
	}

	stats, err := db.Stats(StatsRequest{Workflow: "implement"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.Stages) != 1 {
		t.Fatalf("stage stats = %#v, want one stage", stats.Stages)
	}
	got := stats.Stages[0]
	if got.TotalAttempts != 2 || got.RetryWasteAttempts != 1 ||
		!got.HasRetryWasteDuration || got.RetryWasteDurationMs != 10 ||
		!got.HasRetryWasteTokens || got.RetryWasteTokens != 15 ||
		!got.HasRetryWasteCost || got.RetryWasteCostUSD != 0.5 {
		t.Fatalf("repass retry waste = %#v", got)
	}
}

func TestUsageRollupDoesNotShiftAcrossMissingRepassSpan(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dir := seedUsageRun(t, runsDir, fixtureRunID, "implement", "agent", fixtureStart,
		usageAttemptFixture{number: 1, duration: 10 * time.Millisecond, status: "failure", skipSpan: true},
		usageAttemptFixture{number: 1, duration: 20 * time.Millisecond, status: "success", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 15, telemetry.AttrGenAIUsageOutputTokens: 20,
			telemetry.AttrUsageCostUSD: 1.5,
		}})

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun task repass with missing span: %v", err)
	}

	attempts, err := db.StageAttempts(fixtureRunID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("StageAttempts: %v, %#v", err, attempts)
	}
	if attempts[0].InputTokens != nil || attempts[0].OutputTokens != nil || attempts[0].CostUSD != nil {
		t.Fatalf("missing first span acquired later usage: %#v", attempts[0])
	}
	if attempts[1].InputTokens == nil || *attempts[1].InputTokens != 15 ||
		attempts[1].OutputTokens == nil || *attempts[1].OutputTokens != 20 ||
		attempts[1].CostUSD == nil || *attempts[1].CostUSD != 1.5 {
		t.Fatalf("later repass usage = %#v", attempts[1])
	}

	stats, err := db.Stats(StatsRequest{Workflow: "implement"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	got := stats.Stages[0]
	if got.RetryWasteAttempts != 1 || got.HasRetryWasteTokens || got.HasRetryWasteCost {
		t.Fatalf("missing repass usage became retry-waste zero: %#v", got)
	}
}

func TestUsageRollupIgnoresAttemptlessTerminalFailureDiagnostic(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "runs", fixtureRunID)
	mustMkdirAll(t, filepath.Join(dir, dirSpans))
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), minimalRunYAML(fixtureRunID, fixtureStart))

	started := fixtureStart.Add(time.Millisecond)
	finished := started.Add(10 * time.Millisecond)
	events := []string{
		eventLine(1, fixtureStart, `"type":"run.started"`),
		eventLine(2, started, `"type":"stage.started","stage":"agent","attempt":1`),
		eventLine(3, finished, `"type":"stage.finished","stage":"agent","attempt":1,"status":"failure","error":{"code":"executor_error","message":"session timed out"}`),
		eventLine(4, finished.Add(time.Millisecond), `"type":"error","stage":"agent","error":{"code":"run_failed","message":"executor_error: session timed out"}`),
		eventLine(5, finished.Add(2*time.Millisecond), `"type":"run.finished","status":"failed"`),
	}
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(events, "\n")+"\n")
	mustWriteFile(t, filepath.Join(dir, dirSpans, fileSpans), "")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun terminal stage failure: %v", err)
	}

	attempts, err := db.StageAttempts(fixtureRunID)
	if err != nil {
		t.Fatalf("StageAttempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Traversal != 1 || attempts[0].Attempt != 1 {
		t.Fatalf("terminal stage attempts = %#v, want one real traversal", attempts)
	}
	stats, err := db.Stats(StatsRequest{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.Stages) != 1 || stats.Stages[0].RetryWasteAttempts != 0 {
		t.Fatalf("terminal failure retry waste = %#v, want none", stats.Stages)
	}
	errs, err := db.RunErrors(fixtureRunID)
	if err != nil {
		t.Fatalf("RunErrors: %v", err)
	}
	if len(errs) != 2 || errs[1].Code != "run_failed" || errs[1].Attempt != 0 {
		t.Fatalf("terminal run errors = %#v, want attemptless run_failed retained", errs)
	}
}

func TestUsageRollupDerivesRetryDurationFromExecutorError(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "runs", fixtureRunID)
	mustMkdirAll(t, filepath.Join(dir, dirSpans))
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), minimalRunYAML(fixtureRunID, fixtureStart))

	firstStarted := fixtureStart.Add(time.Millisecond)
	firstFailed := firstStarted.Add(10 * time.Millisecond)
	secondStarted := firstFailed.Add(time.Millisecond)
	secondFinished := secondStarted.Add(20 * time.Millisecond)
	events := []string{
		eventLine(1, fixtureStart, `"type":"run.started"`),
		eventLine(2, firstStarted, `"type":"stage.started","stage":"agent","attempt":1`),
		eventLine(3, firstFailed, `"type":"error","stage":"agent","attempt":1,"error":{"code":"executor_error","message":"provider unavailable"}`),
		eventLine(4, secondStarted, `"type":"stage.started","stage":"agent","attempt":2,"attemptClass":"policy"`),
		eventLine(5, secondFinished, `"type":"stage.finished","stage":"agent","attempt":2,"attemptClass":"policy","status":"success"`),
		eventLine(6, secondFinished.Add(time.Millisecond), `"type":"run.finished","status":"completed"`),
	}
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(events, "\n")+"\n")
	mustWriteFile(t, filepath.Join(dir, dirSpans, fileSpans), "")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun executor retry: %v", err)
	}

	attempts, err := db.StageAttempts(fixtureRunID)
	if err != nil {
		t.Fatalf("StageAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("stage attempts = %#v, want two", attempts)
	}
	if attempts[0].Status != stageStatusFailure || attempts[0].DurationMs != 10 || !attempts[0].FinishedAt.Equal(firstFailed) {
		t.Fatalf("failed retry attempt = %#v, want executor-error boundary and 10ms duration", attempts[0])
	}
	stats, err := db.Stats(StatsRequest{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.Stages) != 1 {
		t.Fatalf("stage stats = %#v, want one stage", stats.Stages)
	}
	got := stats.Stages[0]
	if got.RetryWasteAttempts != 1 || !got.HasRetryWasteDuration || got.RetryWasteDurationMs != 10 {
		t.Fatalf("executor retry waste = %#v, want one attempt and 10ms", got)
	}
}

func TestUsageRollupPercentilesAndRetryWaste(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart
	envelope := apiv1.ResultEnvelope{Metrics: map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  5,
		telemetry.AttrGenAIUsageOutputTokens: 15,
		telemetry.AttrCopilotPremiumRequests: 1,
		telemetry.AttrUsageCostUSD:           1.25,
	}}

	firstDir := seedUsageRun(t, runsDir, "1111111111111111abababababababab", "implement", "agent", base,
		usageAttemptFixture{duration: 10 * time.Millisecond, status: "success", metrics: envelope.Metrics})
	seedUsageRun(t, runsDir, "2222222222222222abababababababab", "implement", "agent", base.Add(time.Hour),
		usageAttemptFixture{duration: 20 * time.Millisecond, status: "failure", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 10, telemetry.AttrGenAIUsageOutputTokens: 30,
			telemetry.AttrUsageCostUSD: 2,
		}},
		usageAttemptFixture{duration: 30 * time.Millisecond, status: "success", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 20, telemetry.AttrGenAIUsageOutputTokens: 40,
			telemetry.AttrUsageCostUSD: 3,
		}})
	seedUsageRun(t, runsDir, "3333333333333333abababababababab", "implement", "agent", base.Add(2*time.Hour),
		usageAttemptFixture{duration: 40 * time.Millisecond, status: "failure"},
		usageAttemptFixture{duration: 50 * time.Millisecond, status: "success", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 40, telemetry.AttrGenAIUsageOutputTokens: 60,
		}})
	seedUsageRun(t, runsDir, "4444444444444444abababababababab", "implement", "fully-retried", base.Add(3*time.Hour),
		usageAttemptFixture{duration: 7 * time.Millisecond, status: "failure", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 2, telemetry.AttrGenAIUsageOutputTokens: 3,
			telemetry.AttrUsageCostUSD: 0.5,
		}},
		usageAttemptFixture{duration: 8 * time.Millisecond, status: "success", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 3, telemetry.AttrGenAIUsageOutputTokens: 4,
			telemetry.AttrUsageCostUSD: 0.75,
		}})
	unmeteredDir := seedUsageRun(t, runsDir, "5555555555555555abababababababab", "implement", "unmetered", base.Add(4*time.Hour),
		usageAttemptFixture{duration: 5 * time.Millisecond, status: "success"})
	seedUsageRun(t, runsDir, "6666666666666666abababababababab", "implement", "zero-metered", base.Add(5*time.Hour),
		usageAttemptFixture{duration: 6 * time.Millisecond, status: "success", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 0, telemetry.AttrGenAIUsageOutputTokens: 0,
			telemetry.AttrCopilotPremiumRequests: 0, telemetry.AttrUsageCostUSD: 0,
		}})
	seedUsageRun(t, runsDir, "7777777777777777abababababababab", "nominate", "agent", base.Add(6*time.Hour),
		usageAttemptFixture{duration: time.Second, status: "success", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens: 1000, telemetry.AttrGenAIUsageOutputTokens: 1000,
			telemetry.AttrUsageCostUSD: 100,
		}})

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	attempts, err := db.StageAttempts("1111111111111111abababababababab")
	if err != nil || len(attempts) != 1 {
		t.Fatalf("StageAttempts: %v, %#v", err, attempts)
	}
	got := attempts[0]
	if got.InputTokens == nil || *got.InputTokens != int64(envelope.Metrics[telemetry.AttrGenAIUsageInputTokens]) ||
		got.OutputTokens == nil || *got.OutputTokens != int64(envelope.Metrics[telemetry.AttrGenAIUsageOutputTokens]) ||
		got.CopilotPremiumRequests == nil || *got.CopilotPremiumRequests != envelope.Metrics[telemetry.AttrCopilotPremiumRequests] ||
		got.CostUSD == nil || *got.CostUSD != envelope.Metrics[telemetry.AttrUsageCostUSD] {
		t.Fatalf("rollup usage = %#v, want envelope metrics %#v", got, envelope.Metrics)
	}

	stats, err := db.Stats(StatsRequest{Workflow: "implement"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	byStage := make(map[string]StageStats, len(stats.Stages))
	for _, stat := range stats.Stages {
		byStage[stat.Stage] = stat
	}
	var workflowUsage UsageStats
	for _, usage := range stats.Usage {
		if usage.Scope == "workflow" && usage.Workflow == "implement" {
			workflowUsage = usage
			break
		}
	}
	if workflowUsage.TotalAttempts != 9 ||
		!workflowUsage.HasTokens || workflowUsage.TokenSamples != 7 ||
		workflowUsage.P50Tokens != 20 || workflowUsage.P95Tokens != 100 ||
		!workflowUsage.HasCost || workflowUsage.CostSamples != 6 ||
		workflowUsage.P50CostUSD != 0.75 || workflowUsage.P95CostUSD != 3 {
		t.Fatalf("workflow usage rollup = %#v", workflowUsage)
	}
	if workflowUsage.RetryWasteAttempts != 3 ||
		workflowUsage.HasRetryWasteTokens || workflowUsage.HasRetryWasteCost {
		t.Fatalf("partial workflow retry usage became a total: %#v", workflowUsage)
	}

	agent := byStage["agent"]
	if agent.DurationSamples != 5 || agent.P50DurationMs != 30 || agent.P95DurationMs != 50 {
		t.Fatalf("agent duration percentiles = %#v", agent)
	}
	if !agent.HasTokens || agent.TokenSamples != 4 || agent.P50Tokens != 40 || agent.P95Tokens != 100 {
		t.Fatalf("agent token percentiles = %#v", agent)
	}
	if !agent.HasCost || agent.CostSamples != 3 || agent.P50CostUSD != 2 || agent.P95CostUSD != 3 {
		t.Fatalf("agent cost percentiles = %#v", agent)
	}
	if agent.RetryWasteAttempts != 2 || !agent.HasRetryWasteDuration || agent.RetryWasteDurationMs != 60 {
		t.Fatalf("agent retry waste = %#v", agent)
	}
	if agent.HasRetryWasteTokens || agent.HasRetryWasteCost {
		t.Fatalf("partial retry usage became a total: %#v", agent)
	}

	retried := byStage["fully-retried"]
	if retried.RetryWasteAttempts != 1 ||
		!retried.HasRetryWasteDuration || retried.RetryWasteDurationMs != 7 ||
		!retried.HasRetryWasteTokens || retried.RetryWasteTokens != 5 ||
		!retried.HasRetryWasteCost || retried.RetryWasteCostUSD != 0.5 {
		t.Fatalf("complete retry waste = %#v", retried)
	}

	unmetered := byStage["unmetered"]
	if unmetered.HasTokens || unmetered.HasCost || unmetered.TokenSamples != 0 || unmetered.CostSamples != 0 {
		t.Fatalf("missing usage became observed: %#v", unmetered)
	}
	missing, err := db.StageAttempts("5555555555555555abababababababab")
	if err != nil || len(missing) != 1 {
		t.Fatalf("unmetered StageAttempts: %v, %#v", err, missing)
	}
	if missing[0].InputTokens != nil || missing[0].OutputTokens != nil ||
		missing[0].CopilotPremiumRequests != nil || missing[0].CostUSD != nil {
		t.Fatalf("missing raw usage became zero: %#v", missing[0])
	}
	zero, err := db.StageAttempts("6666666666666666abababababababab")
	if err != nil || len(zero) != 1 {
		t.Fatalf("zero-metered StageAttempts: %v, %#v", err, zero)
	}
	if zero[0].InputTokens == nil || *zero[0].InputTokens != 0 ||
		zero[0].OutputTokens == nil || *zero[0].OutputTokens != 0 ||
		zero[0].CopilotPremiumRequests == nil || *zero[0].CopilotPremiumRequests != 0 ||
		zero[0].CostUSD == nil || *zero[0].CostUSD != 0 {
		t.Fatalf("reported zero usage became missing: %#v", zero[0])
	}
	zeroStats := byStage["zero-metered"]
	if !zeroStats.HasTokens || zeroStats.TokenSamples != 1 || zeroStats.P50Tokens != 0 ||
		!zeroStats.HasCost || zeroStats.CostSamples != 1 || zeroStats.P50CostUSD != 0 {
		t.Fatalf("reported zero usage was not aggregated: %#v", zeroStats)
	}

	if err := db.IngestRun(firstDir); err != nil {
		t.Fatalf("re-ingest usage run: %v", err)
	}
	if err := db.IngestRun(unmeteredDir); err != nil {
		t.Fatalf("re-ingest unmetered run: %v", err)
	}
}

func TestUsageRollupGroupsObservedUsageByModel(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	firstDir := seedUsageRun(t, runsDir, "1111111111111111cdcdcdcdcdcdcdcd", "implement", "agent", fixtureStart,
		usageAttemptFixture{duration: time.Millisecond, status: "success", modelUsage: []telemetry.ModelUsage{
			{
				Model: "a-model", InputTokens: int64Usage(10), OutputTokens: int64Usage(5),
				CopilotPremiumRequests: float64Usage(0.5), CostUSD: float64Usage(0.25),
			},
			{
				Model: "zero-model", InputTokens: int64Usage(0), OutputTokens: int64Usage(0),
				CopilotPremiumRequests: float64Usage(0), CostUSD: float64Usage(0),
			},
		}})
	seedUsageRun(t, runsDir, "2222222222222222cdcdcdcdcdcdcdcd", "implement", "agent", fixtureStart.Add(time.Hour),
		usageAttemptFixture{duration: time.Millisecond, status: "success", modelUsage: []telemetry.ModelUsage{{
			Model: "a-model", InputTokens: int64Usage(20), OutputTokens: int64Usage(10), CostUSD: float64Usage(0.75),
		}}})
	seedUsageRun(t, runsDir, "3333333333333333cdcdcdcdcdcdcdcd", "implement", "unmeasured", fixtureStart.Add(2*time.Hour),
		usageAttemptFixture{duration: time.Millisecond, status: "success"})
	seedUsageRun(t, runsDir, "4444444444444444cdcdcdcdcdcdcdcd", "nominate", "agent", fixtureStart.Add(3*time.Hour),
		usageAttemptFixture{duration: time.Millisecond, status: "success", modelUsage: []telemetry.ModelUsage{{
			Model: "a-model", InputTokens: int64Usage(1000), OutputTokens: int64Usage(1000), CostUSD: float64Usage(100),
		}}})

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	stats, err := db.Stats(StatsRequest{Workflow: "implement"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.Models) != 2 {
		t.Fatalf("model stats = %#v, want two measured models", stats.Models)
	}
	aModel := stats.Models[0]
	if aModel.Model != "a-model" || aModel.UsageSamples != 2 ||
		!aModel.HasInputTokens || aModel.InputTokenSamples != 2 || aModel.InputTokens != 30 ||
		!aModel.HasOutputTokens || aModel.OutputTokenSamples != 2 || aModel.OutputTokens != 15 ||
		!aModel.HasPremiumRequests || aModel.PremiumRequestSamples != 1 || aModel.CopilotPremiumRequests != 0.5 ||
		!aModel.HasCost || aModel.CostSamples != 2 || aModel.CostUSD != 1 {
		t.Fatalf("a-model stats = %#v", aModel)
	}
	zero := stats.Models[1]
	if zero.Model != "zero-model" ||
		!zero.HasInputTokens || zero.InputTokens != 0 ||
		!zero.HasOutputTokens || zero.OutputTokens != 0 ||
		!zero.HasPremiumRequests || zero.CopilotPremiumRequests != 0 ||
		!zero.HasCost || zero.CostUSD != 0 {
		t.Fatalf("zero-model stats = %#v", zero)
	}

	var rows int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM stage_model_usage`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 4 {
		t.Fatalf("stage_model_usage rows = %d, want 4 including filtered workflow", rows)
	}
	if err := db.IngestRun(firstDir); err != nil {
		t.Fatalf("re-ingest model usage: %v", err)
	}
}

func TestIngestSkipsAgenticGateUsage(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dir := seedUsageRun(t, runsDir, fixtureRunID, "implement", "agent", fixtureStart,
		usageAttemptFixture{duration: time.Millisecond, status: "success", model: "gpt-5.6-sol", harnessVersion: "copilot version 1.2.3", metrics: map[string]float64{
			telemetry.AttrGenAIUsageInputTokens:  10,
			telemetry.AttrGenAIUsageOutputTokens: 20,
			telemetry.AttrUsageCostUSD:           0.25,
		}})

	eventsPath := filepath.Join(dir, fileEvents)
	eventData, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	eventLines := strings.Split(strings.TrimSpace(string(eventData)), "\n")
	eventLines[len(eventLines)-1] = eventLine(4, fixtureStart.Add(3*time.Millisecond),
		`"type":"gate.evaluated","gate":"review","verdict":"approve","target":"agent"`)
	eventLines = append(eventLines, eventLine(5, fixtureStart.Add(4*time.Millisecond),
		`"type":"run.finished","status":"completed"`))
	mustWriteFile(t, eventsPath, strings.Join(eventLines, "\n")+"\n")

	gateSpan := telemetry.SpanRecord{
		Schema:    telemetry.SpanSchema,
		TraceID:   fixtureRunID,
		SpanID:    "00000000000000ff",
		Name:      "gate/review",
		Kind:      telemetry.SpanKindGate,
		StartTime: fixtureStart.Add(2 * time.Millisecond),
		EndTime:   fixtureStart.Add(3 * time.Millisecond),
		Status:    "ok",
		Attributes: map[string]string{
			telemetry.AttrStage:                  "review",
			telemetry.AttrStageType:              telemetry.StageTypeGate,
			telemetry.AttrGateRepassNumber:       "1",
			telemetry.AttrGoober:                 "reviewer",
			telemetry.AttrModel:                  "claude-sonnet-5",
			telemetry.AttrHarnessVersion:         "copilot version 1.2.3",
			telemetry.AttrGenAIUsageInputTokens:  "100",
			telemetry.AttrGenAIUsageOutputTokens: "200",
			telemetry.AttrUsageCostUSD:           "1.5",
		},
	}
	gateData, err := json.Marshal(gateSpan)
	if err != nil {
		t.Fatal(err)
	}
	spansPath := filepath.Join(dir, dirSpans, fileSpans)
	spanData, err := os.ReadFile(spansPath)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, spansPath, string(spanData)+string(gateData)+"\n")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun with agentic gate usage: %v", err)
	}

	attempts, err := db.StageAttempts(fixtureRunID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("StageAttempts: %v, %#v", err, attempts)
	}
	got := attempts[0]
	if got.InputTokens == nil || *got.InputTokens != 10 ||
		got.OutputTokens == nil || *got.OutputTokens != 20 ||
		got.CostUSD == nil || *got.CostUSD != 0.25 {
		t.Fatalf("task usage = %#v, want task metrics without gate usage", got)
	}

	var spanCount, usageCount int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM spans WHERE run_id = ?`, fixtureRunID).Scan(&spanCount); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM stage_usage WHERE run_id = ?`, fixtureRunID).Scan(&usageCount); err != nil {
		t.Fatal(err)
	}
	if spanCount != 2 || usageCount != 1 {
		t.Fatalf("ingested spans/usage = %d/%d, want 2/1", spanCount, usageCount)
	}
	invocations, err := db.AgentInvocations(fixtureRunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 2 {
		t.Fatalf("agent invocations = %#v", invocations)
	}
	for _, invocation := range invocations {
		switch invocation.Kind {
		case telemetry.SpanKindTask:
			if invocation.Model != "gpt-5.6-sol" || invocation.Traversal == nil || *invocation.Traversal != 1 {
				t.Fatalf("task invocation = %#v", invocation)
			}
		case telemetry.SpanKindGate:
			if invocation.Model != "claude-sonnet-5" || invocation.Traversal != nil || invocation.Attempt != nil {
				t.Fatalf("gate invocation = %#v", invocation)
			}
		default:
			t.Fatalf("unexpected invocation = %#v", invocation)
		}
	}
}

func TestStatsFiltersAndGroupsAgentProvenance(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	fixtures := []struct {
		runID, model, version string
	}{
		{"1bf92f3577b34da6a3ce929d0e0e4731", "gpt-5.6-sol", "copilot version 1.2.3"},
		{"2bf92f3577b34da6a3ce929d0e0e4732", "gpt-5.6-sol", "copilot version 1.2.4"},
		{"3bf92f3577b34da6a3ce929d0e0e4733", "claude-sonnet-5", "copilot version 1.2.3"},
	}
	for i, fixture := range fixtures {
		seedUsageRun(
			t, runsDir, fixture.runID, "implement", "agent", fixtureStart.Add(time.Duration(i)*time.Hour),
			usageAttemptFixture{
				duration: time.Millisecond, status: "success",
				model: fixture.model, harnessVersion: fixture.version,
			},
		)
	}
	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	filtered, err := db.Stats(StatsRequest{Model: "gpt-5.6-sol", HarnessVersion: "copilot version 1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Runs) != 1 || filtered.Runs[0].TotalRuns != 1 ||
		len(filtered.Stages) != 1 || filtered.Stages[0].TotalAttempts != 1 {
		t.Fatalf("filtered stats = %#v", filtered)
	}

	grouped, err := db.Stats(StatsRequest{GroupByModel: true, GroupByHarnessVersion: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(grouped.Runs) != 3 || len(grouped.Stages) != 3 {
		t.Fatalf("grouped stats = %#v", grouped)
	}
	for _, stat := range grouped.Stages {
		if stat.Model == "" || stat.HarnessVersion == "" || stat.TotalAttempts != 1 {
			t.Fatalf("grouped stage = %#v", stat)
		}
	}
}

func TestIngestRejectsInvalidUsageSpans(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		attributes map[string]string
		secondSpan bool
	}{
		{
			name: "malformed metric",
			kind: telemetry.SpanKindTask,
			attributes: map[string]string{
				telemetry.AttrStage: "agent", telemetry.AttrAttemptNumber: "1",
				telemetry.AttrUsageCostUSD: "not-a-number",
			},
		},
		{
			name: "non-task span",
			kind: telemetry.SpanKindRun,
			attributes: map[string]string{
				telemetry.AttrStage: "agent", telemetry.AttrAttemptNumber: "1",
				telemetry.AttrGenAIUsageInputTokens: "1",
			},
		},
		{
			name: "unmatched attempt",
			kind: telemetry.SpanKindTask,
			attributes: map[string]string{
				telemetry.AttrStage: "agent", telemetry.AttrAttemptNumber: "2",
				telemetry.AttrGenAIUsageInputTokens: "1",
			},
		},
		{
			name: "duplicate usage span",
			kind: telemetry.SpanKindTask,
			attributes: map[string]string{
				telemetry.AttrStage: "agent", telemetry.AttrAttemptNumber: "1",
				telemetry.AttrGenAIUsageInputTokens: "1",
			},
			secondSpan: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			runsDir := filepath.Join(tmp, "runs")
			dir := seedUsageRun(t, runsDir, fixtureRunID, "implement", "agent", fixtureStart,
				usageAttemptFixture{duration: time.Millisecond, status: "success"})
			records := []telemetry.SpanRecord{{
				Schema: telemetry.SpanSchema, TraceID: fixtureRunID, SpanID: "00000000000000aa",
				Name: "task/agent", Kind: tc.kind, StartTime: fixtureStart, EndTime: fixtureStart.Add(time.Millisecond),
				Status: "ok", Attributes: tc.attributes,
			}}
			if tc.secondSpan {
				duplicate := records[0]
				duplicate.SpanID = "00000000000000bb"
				records = append(records, duplicate)
			}
			var lines []string
			for _, record := range records {
				data, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				lines = append(lines, string(data))
			}
			if err := os.WriteFile(filepath.Join(dir, dirSpans, fileSpans), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			db := openTestDB(t, tmp)
			if err := db.IngestRun(dir); err == nil {
				t.Fatal("IngestRun succeeded with invalid usage span")
			}
		})
	}
}
