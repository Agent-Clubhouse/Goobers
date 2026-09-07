package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

func TestIngestRunCostAttributionAndExactUsageReplacement(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	runID := strings.Repeat("c", 32)
	runDir := writeCostFixture(t, runsDir, runID, []string{
		costRefEvent(6, fixtureStart.Add(5*time.Second), "issue", "41", "claim"),
		costRefEvent(7, fixtureStart.Add(6*time.Second), "pr", "90", "open"),
	}, []costAttemptFixture{
		{attempt: 1, status: "failure", nanoAIU: int64Pointer(0), input: int64Pointer(0), model: "gpt-a"},
		{
			attempt: 2, status: "success", nanoAIU: int64Pointer(125), input: int64Pointer(11),
			cacheRead: int64Pointer(7), cacheWrite: int64Pointer(3), reasoning: int64Pointer(5),
			model: "gpt-b", billingModel: "ai_credits", costBasis: "vendor_reported",
		},
	})
	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatalf("repeat IngestRun: %v", err)
	}

	attributions, err := db.RunCostAttributions(context.Background(), runID)
	if err != nil {
		t.Fatalf("RunCostAttributions: %v", err)
	}
	if len(attributions) != 2 ||
		attributions[0].ExternalKind != "issue" || attributions[0].ExternalID != "41" || attributions[0].Relationship != "claim" ||
		attributions[1].ExternalKind != "pr" || attributions[1].ExternalID != "90" || attributions[1].Relationship != "open" {
		t.Fatalf("attributions = %#v", attributions)
	}
	attempts, err := db.StageAttempts(context.Background(), runID)
	if err != nil {
		t.Fatalf("StageAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("len(attempts) = %d, want 2", len(attempts))
	}
	if attempts[0].NanoAIU == nil || *attempts[0].NanoAIU != 0 || attempts[0].CacheReadTokens != nil {
		t.Fatalf("measured zero/unmeasured dimensions lost: %#v", attempts[0])
	}
	if attempts[1].NanoAIU == nil || *attempts[1].NanoAIU != 125 ||
		attempts[1].CacheReadTokens == nil || *attempts[1].CacheReadTokens != 7 ||
		attempts[1].CacheWriteTokens == nil || *attempts[1].CacheWriteTokens != 3 ||
		attempts[1].ReasoningTokens == nil || *attempts[1].ReasoningTokens != 5 ||
		attempts[1].BillingModel != "ai_credits" || attempts[1].CostBasis != "vendor_reported" {
		t.Fatalf("exact dimensions = %#v", attempts[1])
	}
	var modelNano, modelCacheRead, modelReasoning sql.NullInt64
	var billingModel, costBasis sql.NullString
	if err := db.sql.QueryRow(`
		SELECT nano_aiu, cache_read_tokens, reasoning_tokens, billing_model, cost_basis
		FROM stage_model_usage
		WHERE run_id = ? AND traversal = 2 AND model = 'gpt-b'`, runID).
		Scan(&modelNano, &modelCacheRead, &modelReasoning, &billingModel, &costBasis); err != nil {
		t.Fatalf("query model usage: %v", err)
	}
	if !modelNano.Valid || modelNano.Int64 != 125 || !modelCacheRead.Valid || modelCacheRead.Int64 != 7 ||
		!modelReasoning.Valid || modelReasoning.Int64 != 5 ||
		billingModel.String != "ai_credits" || costBasis.String != "vendor_reported" {
		t.Fatalf("model usage = nano=%v cache=%v reasoning=%v billing=%v basis=%v",
			modelNano, modelCacheRead, modelReasoning, billingModel, costBasis)
	}
	prs, err := db.PullRequestCosts(context.Background(), "github")
	if err != nil {
		t.Fatalf("PullRequestCosts: %v", err)
	}
	if len(prs) != 1 || len(prs[0].Models) != 2 ||
		prs[0].Models[0].Model != "gpt-a" || prs[0].Models[0].NanoAIU == nil || *prs[0].Models[0].NanoAIU != 0 ||
		prs[0].Models[1].Model != "gpt-b" || prs[0].Models[1].NanoAIU == nil || *prs[0].Models[1].NanoAIU != 125 {
		t.Fatalf("model aggregates = %#v", prs)
	}

	writeCostFixture(t, runsDir, runID, []string{
		costRefEvent(4, fixtureStart.Add(3*time.Second), "issue", "42", "update"),
	}, []costAttemptFixture{{attempt: 1, status: "success", nanoAIU: int64Pointer(9), model: "gpt-c"}})
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatalf("replacement IngestRun: %v", err)
	}

	attributions, err = db.RunCostAttributions(context.Background(), runID)
	if err != nil {
		t.Fatalf("replacement RunCostAttributions: %v", err)
	}
	if len(attributions) != 1 || attributions[0].ExternalID != "42" || attributions[0].Relationship != "update" {
		t.Fatalf("replacement attributions = %#v", attributions)
	}
	attempts, err = db.StageAttempts(context.Background(), runID)
	if err != nil {
		t.Fatalf("replacement StageAttempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].NanoAIU == nil || *attempts[0].NanoAIU != 9 {
		t.Fatalf("replacement attempts = %#v", attempts)
	}
}

func TestCostRebuildRestoresAttributionAndUsage(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	runID := strings.Repeat("d", 32)
	writeCostFixture(t, runsDir, runID, []string{
		costRefEvent(4, fixtureStart.Add(3*time.Second), "issue", "7", "claim"),
		costRefEvent(5, fixtureStart.Add(4*time.Second), "pr", "8", "open"),
	}, []costAttemptFixture{{attempt: 1, status: "success", nanoAIU: int64Pointer(44), model: "gpt-r"}})
	dbPath := filepath.Join(tmp, "telemetry.db")
	if err := Rebuild(context.Background(), dbPath, runsDir, filepath.Join(tmp, "scheduler")); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open rebuilt database: %v", err)
	}
	defer func() { _ = db.Close() }()
	prs, err := db.PullRequestCosts(context.Background(), "github")
	if err != nil {
		t.Fatalf("PullRequestCosts: %v", err)
	}
	if len(prs) != 1 || prs[0].NanoAIU == nil || *prs[0].NanoAIU != 44 || prs[0].TotalRuns != 1 {
		t.Fatalf("rebuilt PR costs = %#v", prs)
	}
}

func TestCostAggregatesRetriesSharedAllocationOrphanAndCoverage(t *testing.T) {
	tmp := t.TempDir()
	db := openTestDB(t, tmp)
	seedCostRow(t, db, "run-a", fixtureStart, []costRef{{"issue", "1"}}, []costUsage{{nanoAIU: int64Pointer(100)}})
	seedCostRow(t, db, "run-b", fixtureStart.Add(time.Hour), []costRef{{"issue", "1"}, {"issue", "2"}, {"pr", "10"}}, []costUsage{{nanoAIU: int64Pointer(300)}})
	seedCostRow(t, db, "run-c", fixtureStart.Add(2*time.Hour), []costRef{{"pr", "10"}}, []costUsage{
		{nanoAIU: int64Pointer(50)}, {nanoAIU: int64Pointer(70)},
	})
	seedCostRow(t, db, "run-d", fixtureStart.Add(3*time.Hour), []costRef{{"pr", "10"}}, []costUsage{{}})

	prs, err := db.PullRequestCosts(context.Background(), "github")
	if err != nil {
		t.Fatalf("PullRequestCosts: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("PR aggregates = %#v", prs)
	}
	pr := prs[0]
	if pr.ExternalID != "10" || pr.TotalRuns != 4 || pr.MeasuredRuns != 3 ||
		pr.TotalAttempts != 5 || pr.MeasuredAttempts != 4 || pr.NanoAIU == nil || *pr.NanoAIU != 520 {
		t.Fatalf("PR aggregate = %#v", pr)
	}
	if len(pr.Runs) != 4 || pr.Runs[0].RunID != "run-a" || pr.Runs[3].RunID != "run-d" {
		t.Fatalf("PR run breakdown = %#v", pr.Runs)
	}

	issues, err := db.IssueCosts(context.Background(), "github")
	if err != nil {
		t.Fatalf("IssueCosts: %v", err)
	}
	if len(issues) != 2 || issues[0].ExternalID != "1" || issues[1].ExternalID != "2" {
		t.Fatalf("issue ordering = %#v", issues)
	}
	if issues[0].NanoAIU == nil || *issues[0].NanoAIU != 325 ||
		issues[1].NanoAIU == nil || *issues[1].NanoAIU != 195 {
		t.Fatalf("weighted issue costs = %#v", issues)
	}
	if issues[0].MeasuredRuns != 3 || issues[0].TotalRuns != 4 ||
		issues[1].MeasuredRuns != 2 || issues[1].TotalRuns != 3 {
		t.Fatalf("issue coverage = %#v", issues)
	}
	if len(issues[0].Runs) != 4 || len(issues[1].Runs) != 3 {
		t.Fatalf("issue run breakdown = %#v", issues)
	}
}

func TestCostAggregatesBoundsWindowAndFiltersExternalID(t *testing.T) {
	tmp := t.TempDir()
	db := openTestDB(t, tmp)
	defer func() { _ = db.Close() }()
	seedCostRow(t, db, "before", fixtureStart, []costRef{{"pr", "10"}, {"issue", "1"}}, []costUsage{{nanoAIU: int64Pointer(10)}})
	seedCostRow(t, db, "inside-a", fixtureStart.Add(time.Hour), []costRef{{"pr", "10"}, {"issue", "1"}}, []costUsage{{nanoAIU: int64Pointer(20)}})
	seedCostRow(t, db, "inside-b", fixtureStart.Add(2*time.Hour), []costRef{{"pr", "11"}, {"issue", "2"}}, []costUsage{{nanoAIU: int64Pointer(30)}})
	seedCostRow(t, db, "after", fixtureStart.Add(3*time.Hour), []costRef{{"pr", "10"}, {"issue", "1"}}, []costUsage{{nanoAIU: int64Pointer(40)}})

	result, err := db.CostAggregates(context.Background(), CostQuery{
		Provider: "github", ExternalKind: CostExternalKindPR, ExternalID: "10",
		Since: fixtureStart.Add(30 * time.Minute), Until: fixtureStart.Add(3 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PullRequests) != 1 || len(result.Issues) != 0 ||
		result.PullRequests[0].ExternalID != "10" ||
		result.PullRequests[0].NanoAIU == nil || *result.PullRequests[0].NanoAIU != 20 {
		t.Fatalf("bounded result = %+v", result)
	}
}

func TestCostTargetsAndBreakdownForRun(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	seedCostRow(t, db, "run-issue", fixtureStart, []costRef{{"issue", "7"}}, []costUsage{{nanoAIU: int64Pointer(20_000_000_000)}})
	seedCostRow(t, db, "run-pr", fixtureStart.Add(time.Hour), []costRef{{"issue", "7"}, {"pr", "9"}}, []costUsage{{
		nanoAIU: int64Pointer(2_000_000_000),
	}})
	for _, row := range []struct {
		run, model string
		input      int64
		nano       int64
	}{
		{"run-issue", "claude-sonnet-5", 10, 20_000_000_000},
		{"run-pr", "gpt-5.6-sol", 30, 2_000_000_000},
	} {
		if _, err := db.sql.Exec(`
			INSERT INTO stage_model_usage
				(run_id, stage, traversal, attempt, model, input_tokens, nano_aiu, cost_usd, billing_model, cost_basis)
			VALUES (?, 'implement', 1, 1, ?, ?, ?, ?, 'ai_credits', 'vendor_reported')`,
			row.run, row.model, row.input, row.nano, telemetry.NanoAIUToUSD(row.nano)); err != nil {
			t.Fatalf("insert model usage: %v", err)
		}
	}

	prs, issues, err := db.CostTargetsForRun(context.Background(), "github", "run-pr")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prs, []string{"9"}) || !reflect.DeepEqual(issues, []string{"7"}) {
		t.Fatalf("targets = prs %v issues %v", prs, issues)
	}
	rows, err := db.CostBreakdown(context.Background(), "github", CostExternalKindPR, "9")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].RunID != "run-issue" || rows[1].RunID != "run-pr" {
		t.Fatalf("PR breakdown = %#v", rows)
	}
	issueRows, err := db.CostBreakdown(context.Background(), "github", CostExternalKindIssue, "7")
	if err != nil {
		t.Fatal(err)
	}
	if len(issueRows) != 2 || issueRows[0].Model != "claude-sonnet-5" || issueRows[1].Model != "gpt-5.6-sol" {
		t.Fatalf("issue breakdown = %#v", issueRows)
	}
}

func TestCostMigrationUpgradeAndConcurrentFreshOpen(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "telemetry.db")
	legacy, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_meta (version INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create schema metadata: %v", err)
	}
	for i := 0; i < 21; i++ {
		if _, err := legacy.Exec(migrations[i]); err != nil {
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO schema_meta (version) VALUES (21)`); err != nil {
		t.Fatalf("stamp legacy schema: %v", err)
	}
	if _, err := legacy.Exec(`
		INSERT INTO stage_usage
			(run_id, stage, traversal, attempt, input_tokens, copilot_premium_requests, cost_usd, branch)
		VALUES ('legacy', 'implement', 1, 1, 0, 0, 0, 0)`); err != nil {
		t.Fatalf("insert legacy usage: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade Open: %v", err)
	}
	var input, nanoAIU sql.NullInt64
	if err := upgraded.sql.QueryRow(`SELECT input_tokens, nano_aiu FROM stage_usage WHERE run_id = 'legacy'`).Scan(&input, &nanoAIU); err != nil {
		t.Fatalf("query upgraded usage: %v", err)
	}
	if !input.Valid || input.Int64 != 0 || nanoAIU.Valid {
		t.Fatalf("upgraded usage = input %v nanoAIU %v", input, nanoAIU)
	}
	_ = upgraded.Close()

	freshPath := filepath.Join(tmp, "concurrent.db")
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Open(freshPath)
			if err == nil {
				err = db.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
}

type costAttemptFixture struct {
	attempt                                 int
	status, model, billingModel, costBasis  string
	input, cacheRead, cacheWrite, reasoning *int64
	nanoAIU                                 *int64
}

func writeCostFixture(t *testing.T, runsDir, runID string, refs []string, attempts []costAttemptFixture) string {
	t.Helper()
	var events []string
	var spans []string
	seq := 1
	events = append(events, eventLine(seq, fixtureStart, `"type":"run.started"`))
	seq++
	for i, attempt := range attempts {
		start := fixtureStart.Add(time.Duration(seq) * time.Second)
		events = append(events, eventLine(seq, start, fmt.Sprintf(`"type":"stage.started","stage":"implement","attempt":%d`, attempt.attempt)))
		seq++
		finish := fixtureStart.Add(time.Duration(seq) * time.Second)
		events = append(events, eventLine(seq, finish, fmt.Sprintf(`"type":"stage.finished","stage":"implement","attempt":%d,"status":%q`, attempt.attempt, attempt.status)))
		seq++
		attrs := map[string]string{
			telemetry.AttrStage:         "implement",
			telemetry.AttrAttemptNumber: fmt.Sprint(attempt.attempt),
			telemetry.AttrBranch:        "0",
		}
		setIntAttr(attrs, telemetry.AttrGenAIUsageInputTokens, attempt.input)
		setIntAttr(attrs, telemetry.AttrUsageCacheReadTokens, attempt.cacheRead)
		setIntAttr(attrs, telemetry.AttrUsageCacheWriteTokens, attempt.cacheWrite)
		setIntAttr(attrs, telemetry.AttrUsageReasoningTokens, attempt.reasoning)
		setIntAttr(attrs, telemetry.AttrUsageNanoAIU, attempt.nanoAIU)
		if attempt.billingModel != "" {
			attrs[telemetry.AttrUsageBillingModel] = attempt.billingModel
		}
		if attempt.costBasis != "" {
			attrs[telemetry.AttrUsageCostBasis] = attempt.costBasis
		}
		var modelEvents []telemetry.SpanEventRecord
		if attempt.model != "" {
			modelAttrs := make(map[string]string, len(attrs)+1)
			for key, value := range attrs {
				if telemetry.IsCanonicalAgentUsageMetric(key) {
					modelAttrs[key] = value
				}
			}
			modelAttrs[telemetry.AttrGenAIResponseModel] = attempt.model
			modelEvents = []telemetry.SpanEventRecord{{
				Name: telemetry.GenAIModelUsageEventName, Time: finish, Attributes: modelAttrs,
			}}
		}
		record := telemetry.SpanRecord{
			Schema: telemetry.SpanSchema, TraceID: runID, SpanID: fmt.Sprintf("%016x", i+1),
			Name: "task/implement", Kind: telemetry.SpanKindTask,
			StartTime: start.Add(-time.Millisecond), EndTime: finish.Add(time.Millisecond), Status: "ok",
			Attributes: attrs, Events: modelEvents,
		}
		body, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal span: %v", err)
		}
		spans = append(spans, string(body))
	}
	events = append(events, refs...)
	events = append(events, eventLine(seq+len(refs), fixtureStart.Add(time.Duration(seq+len(refs))*time.Second), `"type":"run.finished","status":"completed"`))
	return writeRunWithRawEvents(t, runsDir, runID, strings.Join(events, "\n")+"\n", strings.Join(spans, "\n")+"\n")
}

func costRefEvent(seq int, at time.Time, kind, id, operation string) string {
	return eventLine(seq, at, fmt.Sprintf(
		`"type":"ref.touched","externalRef":{"provider":"github","kind":%q,"id":%q},"runner":{"operation":%q}`,
		kind, id, operation))
}

func setIntAttr(attrs map[string]string, key string, value *int64) {
	if value != nil {
		attrs[key] = fmt.Sprint(*value)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

type costRef struct {
	kind, id string
}

type costUsage struct {
	nanoAIU *int64
}

func seedCostRow(t *testing.T, db *DB, runID string, started time.Time, refs []costRef, usage []costUsage) {
	t.Helper()
	if _, err := db.sql.Exec(`
		INSERT INTO runs (run_id, workflow, workflow_version, gaggle, started_at)
		VALUES (?, 'implement', 1, 'test', ?)`, runID, formatTime(started)); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	for _, ref := range refs {
		if _, err := db.sql.Exec(`
			INSERT INTO run_cost_attribution
				(run_id, provider, external_kind, external_id, relationship)
			VALUES (?, 'github', ?, ?, 'touched')`, runID, ref.kind, ref.id); err != nil {
			t.Fatalf("insert attribution: %v", err)
		}
	}
	for i, item := range usage {
		traversal := i + 1
		if _, err := db.sql.Exec(`
			INSERT INTO stage_attempts (run_id, stage, traversal, attempt, status, branch)
			VALUES (?, 'implement', ?, ?, 'success', 0)`, runID, traversal, traversal); err != nil {
			t.Fatalf("insert attempt: %v", err)
		}
		if item.nanoAIU == nil {
			continue
		}
		if _, err := db.sql.Exec(`
			INSERT INTO stage_usage
				(run_id, stage, traversal, attempt, nano_aiu, cost_usd, billing_model, cost_basis, branch)
			VALUES (?, 'implement', ?, ?, ?, ?, 'ai_credits', 'vendor_reported', 0)`,
			runID, traversal, traversal, *item.nanoAIU, telemetry.NanoAIUToUSD(*item.nanoAIU)); err != nil {
			t.Fatalf("insert usage: %v", err)
		}
	}
}
