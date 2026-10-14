package readservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

type windowedAttributionReader struct {
	readmodel.Reader
	rows []readmodel.RunRow
}

func (r windowedAttributionReader) ListRuns(_ context.Context, options readmodel.ListOptions) (readmodel.ListPage, error) {
	var page readmodel.ListPage
	for _, row := range r.rows {
		if options.Gaggle != "" && row.Gaggle != options.Gaggle {
			continue
		}
		if row.StartedAt.Before(options.Since) || !row.StartedAt.Before(options.Until) {
			continue
		}
		page.Runs = append(page.Runs, row)
	}
	return page, nil
}

type gaggleFixtureRun struct {
	runID, workflow string
	startedAt       time.Time
	phase           journal.RunPhase
	sign            float64
	stages          map[string]float64
	causes          []creditgraph.CauseFinding
	refs            []journal.ExternalRef
	continuedFrom   string
}

func writeGaggleFixtureRun(t *testing.T, root string, fixture gaggleFixtureRun) readmodel.RunRow {
	t.Helper()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: fixture.runID, Workflow: fixture.workflow, WorkflowVersion: 1,
		WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goober", Gaggle: "web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, StartedAt: fixture.startedAt,
		ContinuedFromRunID: fixture.continuedFrom,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "act", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	for i := range fixture.refs {
		if err := run.Append(journal.Event{Type: journal.EventRefTouched, Stage: "act", ExternalRef: &fixture.refs[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	attribution := creditgraph.Attribution{OutcomeSign: fixture.sign, Causes: fixture.causes}
	for stage, share := range fixture.stages {
		attribution.Contributions = append(attribution.Contributions, creditgraph.Contribution{
			NodeID: "stage:" + stage, Kind: creditgraph.KindStage, Stage: stage, Share: share,
		})
	}
	data, err := json.Marshal(creditgraph.RunRecord{
		Schema: creditgraph.RecordSchemaVersion, Status: creditgraph.RecordComplete,
		RunID: fixture.runID, Gaggle: "web", Workflow: fixture.workflow, EffectiveVersion: "v1",
		Workload: "issue", Attribution: attribution,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteFileAtomic(filepath.Join(run.Dir(), creditgraph.RecordFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	finished := fixture.startedAt.Add(time.Minute)
	return readmodel.RunRow{
		RunID: fixture.runID, Gaggle: "web", Workflow: fixture.workflow, Phase: fixture.phase,
		Terminal: true, StartedAt: fixture.startedAt, FinishedAt: &finished,
	}
}

func TestStoredGaggleBackpropReport(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	issue := journal.ExternalRef{Provider: "github", Kind: "issue", ID: "42", URL: "https://example.test/issues/42"}
	// Same URL, different normative identity: must never link.
	lookalike := journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42", URL: "https://example.test/issues/42"}
	cause := creditgraph.CauseFinding{Class: creditgraph.ClassWeakInstructions, Stage: "implement", NodeID: "stage:implement"}
	fixtures := []gaggleFixtureRun{
		{runID: "plan-1", workflow: "plan", startedAt: now.Add(-3 * time.Hour), phase: journal.PhaseCompleted, sign: 1,
			stages: map[string]float64{"plan": 1}, refs: []journal.ExternalRef{issue}},
		{runID: "impl-1", workflow: "implement", startedAt: now.Add(-2 * time.Hour), phase: journal.PhaseFailed, sign: -1,
			stages: map[string]float64{"implement": 3, "review": 1}, causes: []creditgraph.CauseFinding{cause, cause},
			refs: []journal.ExternalRef{issue}},
		{runID: "impl-2", workflow: "implement", startedAt: now.Add(-time.Hour), phase: journal.PhaseFailed, sign: -1,
			stages: map[string]float64{"implement": 1}, causes: []creditgraph.CauseFinding{cause},
			continuedFrom: "impl-1"},
		{runID: "review-1", workflow: "review", startedAt: now.Add(-30 * time.Minute), phase: journal.PhaseFailed, sign: -1,
			stages: map[string]float64{"review": 1}, refs: []journal.ExternalRef{lookalike}, continuedFrom: "impl-2"},
		{runID: "impl-old", workflow: "implement", startedAt: now.Add(-8 * 24 * time.Hour), phase: journal.PhaseCompleted, sign: 1,
			stages: map[string]float64{"implement": 1}},
	}
	reader := windowedAttributionReader{}
	for _, fixture := range fixtures {
		reader.rows = append(reader.rows, writeGaggleFixtureRun(t, root, fixture))
	}
	reader.rows = append(reader.rows, readmodel.RunRow{RunID: "other-gaggle", Gaggle: "api", Terminal: true, StartedAt: now.Add(-time.Hour)})

	report, err := StoredGaggleBackprop(context.Background(), root, reader, GaggleBackpropQuery{Gaggle: "web"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Schema != GaggleBackpropReportSchema || report.Runs != 4 || report.Truncated {
		t.Fatalf("report header = %+v", report)
	}
	workflows := map[string]GaggleWorkflowCredit{}
	for _, workflow := range report.Workflows {
		workflows[workflow.Workflow] = workflow
	}
	if got := workflows["plan"]; got.CreditShare != 1 || got.BlameShare != 0 || got.DownstreamFailures != 1 {
		t.Fatalf("plan credit = %+v, want all credit and one downstream failure", got)
	}
	if got := workflows["implement"]; got.Runs != 2 || got.Failed != 2 || got.BlameShare != 0.6667 || got.DownstreamFailures != 1 {
		t.Fatalf("implement credit = %+v", got)
	}
	stages := map[string]GaggleStageCredit{}
	for _, stage := range report.Stages {
		stages[stage.Workflow+"/"+stage.Stage] = stage
	}
	// impl-1 splits its unit 0.75/0.25; impl-2 and review-1 each give one unit.
	if got := stages["implement/implement"]; got.BlameShare != 0.5833 || got.Runs != 2 {
		t.Fatalf("implement stage = %+v", got)
	}
	if got := stages["implement/review"]; got.BlameShare != 0.0833 {
		t.Fatalf("implement review stage = %+v", got)
	}
	if len(report.RecurringFindings) != 1 || report.RecurringFindings[0].Runs != 2 ||
		report.RecurringFindings[0].Class != creditgraph.ClassWeakInstructions {
		t.Fatalf("recurring findings = %+v, want one finding counted once per run", report.RecurringFindings)
	}
	// impl-2 continues impl-1 within one workflow (a retry): no edge. review-1
	// continues impl-2 across workflows: an edge.
	wantLinks := []GaggleCrossWorkflowLink{
		{Basis: GaggleLinkContinuedFrom, Identifier: "impl-2", FromRunID: "impl-2", FromWorkflow: "implement",
			ToRunID: "review-1", ToWorkflow: "review", ToPhase: journal.PhaseFailed},
		{Basis: GaggleLinkSharedExternalRef, Identifier: "github/issue/42", FromRunID: "plan-1", FromWorkflow: "plan",
			ToRunID: "impl-1", ToWorkflow: "implement", ToPhase: journal.PhaseFailed},
	}
	if len(report.Links) != len(wantLinks) {
		t.Fatalf("links = %+v, want %+v", report.Links, wantLinks)
	}
	for i := range wantLinks {
		if report.Links[i] != wantLinks[i] {
			t.Fatalf("link %d = %+v, want %+v", i, report.Links[i], wantLinks[i])
		}
	}
	trends := map[string]GaggleWorkflowTrend{}
	for _, trend := range report.Trends {
		trends[trend.Workflow] = trend
	}
	if got := trends["implement"]; got.PreviousRuns != 1 || got.PreviousFailureRate != 0 || got.FailureRate != 1 {
		t.Fatalf("implement trend = %+v", got)
	}
}

func TestStoredGaggleBackpropFlagsPartialData(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	fixtures := []gaggleFixtureRun{
		{runID: "impl-1", workflow: "implement", startedAt: now.Add(-2 * time.Hour), phase: journal.PhaseAborted, sign: -1,
			stages: map[string]float64{"implement": 1}},
		{runID: "impl-esc", workflow: "implement", startedAt: now.Add(-time.Hour), phase: journal.PhaseEscalated, sign: 0,
			stages: map[string]float64{"implement": 1}},
		{runID: "old-1", workflow: "implement", startedAt: now.Add(-8 * 24 * time.Hour), phase: journal.PhaseCompleted, sign: 1},
		{runID: "old-2", workflow: "implement", startedAt: now.Add(-9 * 24 * time.Hour), phase: journal.PhaseCompleted, sign: 1},
		{runID: "old-3", workflow: "implement", startedAt: now.Add(-10 * 24 * time.Hour), phase: journal.PhaseCompleted, sign: 1},
	}
	reader := windowedAttributionReader{}
	for _, fixture := range fixtures {
		reader.rows = append(reader.rows, writeGaggleFixtureRun(t, root, fixture))
	}
	runDir, err := instance.NewLayout(root).FindRunDir("impl-esc")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "events.jsonl"), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := StoredGaggleBackprop(context.Background(), root, reader, GaggleBackpropQuery{Gaggle: "web", Limit: 3}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Truncated || !report.PreviousTruncated {
		t.Fatalf("truncation = current %v previous %v, want only the previous window truncated", report.Truncated, report.PreviousTruncated)
	}
	if len(report.LinkEvidenceUnavailable) != 1 || report.LinkEvidenceUnavailable[0] != "impl-esc" {
		t.Fatalf("link evidence unavailable = %v, want [impl-esc]", report.LinkEvidenceUnavailable)
	}
	if len(report.Workflows) != 1 {
		t.Fatalf("workflows = %+v", report.Workflows)
	}
	if got := report.Workflows[0]; got.Failed != 1 || got.Neutral != 1 || got.BlameShare != 1 || got.CreditShare != 0 {
		t.Fatalf("implement credit = %+v, want aborted as the only failure and escalation neutral", got)
	}
	if len(report.Stages) != 1 || report.Stages[0].Runs != 1 || report.Stages[0].BlameShare != 1 {
		t.Fatalf("stages = %+v, want the neutral run excluded", report.Stages)
	}
}

func TestStoredGaggleBackpropRequiresGaggle(t *testing.T) {
	if _, err := StoredGaggleBackprop(context.Background(), t.TempDir(), windowedAttributionReader{}, GaggleBackpropQuery{}, time.Now()); err == nil {
		t.Fatal("report without a gaggle succeeded")
	}
}
