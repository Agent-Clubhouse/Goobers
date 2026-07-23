package rollup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedStatsRun writes a run with a "build" stage and an optional second stage
// (failing with errCode when set), for stats/error aggregate tests. It reuses
// the same hand-written-JSON approach as fixture_test.go (not the package's
// own mirror types) for the same drift-catching reason.
func seedStatsRun(t *testing.T, runsDir, runID, workflow, runStatus string, startedAt time.Time, secondStageFails bool, errCode string) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: "+workflow))

	lines := []string{
		eventLine(1, startedAt, `"type":"run.started"`),
		eventLine(2, startedAt.Add(time.Second), `"type":"stage.started","stage":"build","attempt":1`),
		eventLine(3, startedAt.Add(2*time.Second), `"type":"stage.finished","stage":"build","attempt":1,"status":"success"`),
	}
	seq := 4
	offset := 3
	if secondStageFails {
		lines = append(lines,
			eventLine(seq, startedAt.Add(time.Duration(offset)*time.Second), `"type":"stage.started","stage":"deploy","attempt":1`),
			eventLine(seq+1, startedAt.Add(time.Duration(offset+1)*time.Second), `"type":"error","stage":"deploy","attempt":1,"error":{"code":"`+errCode+`","message":"seeded failure `+errCode+`"}`),
			eventLine(seq+2, startedAt.Add(time.Duration(offset+2)*time.Second), `"type":"stage.finished","stage":"deploy","attempt":1,"status":"failure"`),
		)
		seq += 3
		offset += 3
	}
	lines = append(lines, eventLine(seq, startedAt.Add(time.Duration(offset)*time.Second), `"type":"run.finished","status":"`+runStatus+`"`))
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
}

func seedRefTouched(t *testing.T, runsDir, runID string, seq int, ts time.Time, id, operation string) {
	t.Helper()
	path := filepath.Join(runsDir, runID, fileEvents)
	line := eventLine(seq, ts, `"type":"ref.touched","externalRef":{"provider":"github","kind":"issue","id":"`+id+`"},"runner":{"operation":"`+operation+`"}`) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append ref.touched: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("append ref.touched: %v", err)
	}
}

func seedAndIngest(t *testing.T, db *DB, runsDir string) {
	t.Helper()
	dirs, err := runDirs(runsDir)
	if err != nil {
		t.Fatalf("runDirs: %v", err)
	}
	for _, dir := range dirs {
		if err := db.IngestRun(dir); err != nil {
			t.Fatalf("IngestRun(%s): %v", dir, err)
		}
	}
}

func TestStatsAggregatesByWorkflowAndStage(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// implement: two completed runs (build only) + one failed run (build ok, deploy fails).
	seedStatsRun(t, runsDir, "1111111111111111aaaaaaaaaaaaaaaa", "implement", "completed", base, false, "")
	seedStatsRun(t, runsDir, "2222222222222222aaaaaaaaaaaaaaaa", "implement", "completed", base.Add(time.Hour), false, "")
	seedStatsRun(t, runsDir, "3333333333333333aaaaaaaaaaaaaaaa", "implement", "failed", base.Add(2*time.Hour), true, "provider.rate_limit")
	// nominate: one completed run, different stage name, outside the implement filter.
	seedStatsRun(t, runsDir, "4444444444444444aaaaaaaaaaaaaaaa", "nominate", "completed", base.Add(3*time.Hour), false, "")

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	all, err := db.Stats(StatsRequest{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if len(all.Runs) != 2 {
		t.Fatalf("len(Runs) = %d, want 2 (implement, nominate)", len(all.Runs))
	}
	if len(all.Gaggles) != 1 || all.Gaggles[0].Gaggle != "web" || all.Gaggles[0].TotalRuns != 4 {
		t.Fatalf("gaggle stats = %#v", all.Gaggles)
	}
	var implement RunStats
	for _, r := range all.Runs {
		if r.Workflow == "implement" {
			implement = r
		}
	}
	if implement.TotalRuns != 3 || implement.CompletedRuns != 2 || implement.FailedRuns != 1 {
		t.Fatalf("implement run stats = %#v", implement)
	}
	if implement.Gaggle != "web" {
		t.Fatalf("implement gaggle = %q, want web", implement.Gaggle)
	}
	if got, want := implement.SuccessRate, 2.0/3.0; got != want {
		t.Fatalf("implement SuccessRate = %v, want %v", got, want)
	}

	var implementBuild, nominateBuild, deployStage StageStats
	for _, s := range all.Stages {
		switch {
		case s.Workflow == "implement" && s.Stage == "build":
			implementBuild = s
		case s.Workflow == "nominate" && s.Stage == "build":
			nominateBuild = s
		case s.Workflow == "implement" && s.Stage == "deploy":
			deployStage = s
		}
	}
	if implementBuild.TotalAttempts != 3 || implementBuild.SucceededAttempts != 3 {
		t.Fatalf("implement build stage stats = %#v", implementBuild)
	}
	if nominateBuild.TotalAttempts != 1 || nominateBuild.SucceededAttempts != 1 {
		t.Fatalf("nominate build stage stats = %#v", nominateBuild)
	}
	if deployStage.TotalAttempts != 1 || deployStage.FailedAttempts != 1 || deployStage.SuccessRate != 0 {
		t.Fatalf("deploy stage stats = %#v", deployStage)
	}
	if deployStage.Gaggle != "web" || deployStage.Workflow != "implement" {
		t.Fatalf("deploy stage identity = %#v", deployStage)
	}

	// Filtered by workflow: only implement's 3 runs / build+deploy stages.
	filtered, err := db.Stats(StatsRequest{Workflow: "implement"})
	if err != nil {
		t.Fatalf("Stats filtered: %v", err)
	}
	if len(filtered.Runs) != 1 || filtered.Runs[0].TotalRuns != 3 {
		t.Fatalf("filtered runs = %#v", filtered.Runs)
	}

	// Time-window filtered: exclude everything after +30m -> only the first
	// implement run (base) qualifies (the second starts at +1h).
	windowed, err := db.Stats(StatsRequest{Workflow: "implement", Until: base.Add(30 * time.Minute)})
	if err != nil {
		t.Fatalf("Stats windowed: %v", err)
	}
	if len(windowed.Runs) != 1 || windowed.Runs[0].TotalRuns != 1 {
		t.Fatalf("windowed runs = %#v", windowed.Runs)
	}
}

func TestRebuildAllAndStatsFilterByGaggle(t *testing.T) {
	tmp := t.TempDir()
	alphaRuns := filepath.Join(tmp, "gaggles", "alpha", "runs")
	betaRuns := filepath.Join(tmp, "gaggles", "beta", "runs")
	alphaID := "1111111111111111cccccccccccccccc"
	betaID := "2222222222222222cccccccccccccccc"
	seedStatsRun(t, alphaRuns, alphaID, "implement", "completed", fixtureStart, false, "")
	seedStatsRun(t, betaRuns, betaID, "implement", "failed", fixtureStart.Add(time.Hour), true, "harness.crash")
	for _, fixture := range []struct {
		runsDir string
		runID   string
		gaggle  string
	}{
		{alphaRuns, alphaID, "alpha"},
		{betaRuns, betaID, "beta"},
	} {
		path := filepath.Join(fixture.runsDir, fixture.runID, fileRunYAML)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, path, strings.Replace(string(data), "gaggle: web", "gaggle: "+fixture.gaggle, 1))
	}

	dbPath := filepath.Join(tmp, "telemetry.db")
	if err := RebuildAll(dbPath, []string{alphaRuns, betaRuns}, filepath.Join(tmp, "scheduler")); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	all, err := db.Stats(StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Gaggles) != 2 ||
		all.Gaggles[0].Gaggle != "alpha" ||
		all.Gaggles[0].CompletedRuns != 1 ||
		all.Gaggles[1].Gaggle != "beta" ||
		all.Gaggles[1].FailedRuns != 1 {
		t.Fatalf("all gaggle stats = %+v", all.Gaggles)
	}

	alpha, err := db.Stats(StatsRequest{Gaggle: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alpha.Runs) != 1 || alpha.Runs[0].TotalRuns != 1 || alpha.Runs[0].CompletedRuns != 1 {
		t.Fatalf("alpha stats = %+v", alpha.Runs)
	}
	beta, err := db.Stats(StatsRequest{Gaggle: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if len(beta.Runs) != 1 || beta.Runs[0].TotalRuns != 1 || beta.Runs[0].FailedRuns != 1 {
		t.Fatalf("beta stats = %+v", beta.Runs)
	}
}

func TestErrorsQueryFiltersAndOrders(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart
	seedStatsRun(t, runsDir, "1111111111111111bbbbbbbbbbbbbbbb", "implement", "failed", base, true, "provider.rate_limit")
	seedStatsRun(t, runsDir, "2222222222222222bbbbbbbbbbbbbbbb", "implement", "failed", base.Add(time.Hour), true, "harness.crash")

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	all, err := db.Errors(ErrorsRequest{})
	if err != nil {
		t.Fatalf("Errors: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(errors) = %d, want 2", len(all))
	}
	// Newest first.
	if all[0].RunID != "2222222222222222bbbbbbbbbbbbbbbb" || all[1].RunID != "1111111111111111bbbbbbbbbbbbbbbb" {
		t.Fatalf("errors not newest-first: %#v", all)
	}
	if all[0].Workflow != "implement" || all[0].Stage != "deploy" || all[0].Attempt != 1 {
		t.Fatalf("unexpected error run/stage ref: %#v", all[0])
	}

	rateLimitOnly, err := db.Errors(ErrorsRequest{ErrorClass: "provider-rate-limit"})
	if err != nil {
		t.Fatalf("Errors filtered: %v", err)
	}
	if len(rateLimitOnly) != 1 || rateLimitOnly[0].Code != "provider.rate_limit" {
		t.Fatalf("rate-limit-filtered errors = %#v", rateLimitOnly)
	}

	windowed, err := db.Errors(ErrorsRequest{Until: base.Add(30 * time.Minute)})
	if err != nil {
		t.Fatalf("Errors windowed: %v", err)
	}
	if len(windowed) != 1 || windowed[0].Code != "provider.rate_limit" {
		t.Fatalf("windowed errors = %#v", windowed)
	}

	firstPage, err := db.Errors(ErrorsRequest{Limit: 1})
	if err != nil {
		t.Fatalf("Errors first page: %v", err)
	}
	secondPage, err := db.Errors(ErrorsRequest{
		Limit: 1,
		Cursor: &ErrorCursor{
			OrderTimestamp: firstPage[0].OrderTimestamp,
			RunID:          firstPage[0].RunID,
			Sequence:       firstPage[0].Sequence,
		},
	})
	if err != nil {
		t.Fatalf("Errors second page: %v", err)
	}
	if len(secondPage) != 1 || secondPage[0].Code != "provider.rate_limit" {
		t.Fatalf("second page errors = %#v", secondPage)
	}
}

func TestStatsPreserveUnknownMetrics(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	runID := "1111111111111111eeeeeeeeeeeeeeee"
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), minimalRunYAML(runID, fixtureStart))
	events := strings.Join([]string{
		eventLine(1, fixtureStart, `"type":"run.started"`),
		eventLine(2, fixtureStart.Add(time.Second), `"type":"stage.started","stage":"active","attempt":1`),
	}, "\n") + "\n"
	mustWriteFile(t, filepath.Join(dir, fileEvents), events)

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatal(err)
	}
	stats, err := db.Stats(StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Runs) != 1 || stats.Runs[0].HasDuration ||
		stats.Runs[0].CompletedRuns != 0 || stats.Runs[0].FailedRuns != 0 {
		t.Fatalf("run stats = %+v", stats.Runs)
	}
	if len(stats.Stages) != 1 || stats.Stages[0].HasDuration ||
		stats.Stages[0].SucceededAttempts != 0 || stats.Stages[0].FailedAttempts != 0 {
		t.Fatalf("stage stats = %+v", stats.Stages)
	}
}

func TestErrorsOrderingBreaksTimestampTiesDeterministically(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	firstRunID := "1111111111111111ffffffffffffffff"
	seedStatsRun(t, runsDir, firstRunID, "implement", "failed", fixtureStart, true, "first")
	seedStatsRun(t, runsDir, "2222222222222222ffffffffffffffff", "implement", "failed", fixtureStart, true, "second")
	eventsPath := filepath.Join(runsDir, firstRunID, fileEvents)
	events, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.WriteString(eventLine(
		8,
		fixtureStart.Add(4*time.Second),
		`"type":"error","stage":"deploy","attempt":1,"error":{"code":"first-later-seq"}`,
	) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	errs, err := db.Errors(ErrorsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 3 ||
		errs[0].Code != "second" ||
		errs[1].Code != "first-later-seq" ||
		errs[2].Code != "first" {
		t.Fatalf("timestamp-tied errors = %#v", errs)
	}

	var pageCodes []string
	var cursor *ErrorCursor
	for {
		page, err := db.Errors(ErrorsRequest{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		pageCodes = append(pageCodes, page[0].Code)
		cursor = &ErrorCursor{
			OrderTimestamp: page[0].OrderTimestamp,
			RunID:          page[0].RunID,
			Sequence:       page[0].Sequence,
		}
	}
	if got := strings.Join(pageCodes, ","); got != "second,first-later-seq,first" {
		t.Fatalf("paginated timestamp ties = %q", got)
	}
}

func TestTopErrorSignaturesAggregatesAcrossRuns(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart
	seedStatsRun(t, runsDir, "1111111111111111cccccccccccccccc", "implement", "failed", base, true, "provider.rate_limit")
	seedStatsRun(t, runsDir, "2222222222222222cccccccccccccccc", "implement", "failed", base.Add(time.Hour), true, "provider.rate_limit")
	reviewRunID := "3333333333333333cccccccccccccccc"
	seedStatsRun(t, runsDir, reviewRunID, "implement", "failed", base.Add(2*time.Hour), true, "harness.crash")
	reviewEventsPath := filepath.Join(runsDir, reviewRunID, fileEvents)
	reviewEvents, err := os.ReadFile(reviewEventsPath)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, reviewEventsPath, strings.ReplaceAll(string(reviewEvents), `"stage":"deploy"`, `"stage":"review"`))

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	sigs, err := db.TopErrorSignatures(StatsRequest{}, 10)
	if err != nil {
		t.Fatalf("TopErrorSignatures: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("len(sigs) = %d, want 2", len(sigs))
	}
	// Most frequent first.
	top := sigs[0]
	if top.Code != "provider.rate_limit" || top.Count != 2 {
		t.Fatalf("top signature = %#v", top)
	}
	if top.ExampleRunID == "" || top.ExampleStage != "deploy" {
		t.Fatalf("top signature missing example ref: %#v", top)
	}

	deploy, err := db.TopErrorSignatures(StatsRequest{Stage: "deploy"}, 10)
	if err != nil {
		t.Fatalf("TopErrorSignatures stage filtered: %v", err)
	}
	if len(deploy) != 1 || deploy[0].Code != "provider.rate_limit" || deploy[0].Count != 2 {
		t.Fatalf("deploy signatures = %#v", deploy)
	}

	matching, err := db.Errors(ErrorsRequest{
		Stage:      "deploy",
		Code:       "provider.rate_limit",
		FilterCode: true,
	})
	if err != nil {
		t.Fatalf("Errors signature filtered: %v", err)
	}
	if len(matching) != 2 {
		t.Fatalf("matching signature errors = %#v", matching)
	}
}

func TestTopErrorSignaturesAllowsUnclassifiedSchedulerErrors(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, []string{
		instanceEventLine(1, "error", `"error":{"message":"uncoded scheduler failure"}`),
	}); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatal(err)
	}

	signatures, err := db.TopErrorSignatures(StatsRequest{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(signatures) != 1 ||
		signatures[0].Code != "" ||
		signatures[0].ErrorClass != "" ||
		signatures[0].Count != 1 ||
		signatures[0].ExampleRunID != "" {
		t.Fatalf("unclassified scheduler signatures = %#v", signatures)
	}

	errors, err := db.Errors(ErrorsRequest{
		Code:             "",
		ErrorClass:       "",
		FilterCode:       true,
		FilterErrorClass: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(errors) != 1 ||
		errors[0].RunID != "" ||
		errors[0].Workflow != "" ||
		errors[0].Message != "uncoded scheduler failure" {
		t.Fatalf("unclassified scheduler errors = %#v", errors)
	}
}

func TestProviderMutationCountsGroupsByShape(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart
	seedStatsRun(t, runsDir, "1111111111111111dddddddddddddddd", "implement", "completed", base, false, "")
	seedStatsRun(t, runsDir, "2222222222222222dddddddddddddddd", "implement", "completed", base.Add(time.Hour), false, "")
	seedStatsRun(t, runsDir, "3333333333333333dddddddddddddddd", "implement", "completed", base.Add(2*time.Hour), false, "")
	seedRefTouched(t, runsDir, "1111111111111111dddddddddddddddd", 4, base.Add(3*time.Second), "42", "claim")
	seedRefTouched(t, runsDir, "2222222222222222dddddddddddddddd", 4, base.Add(time.Hour+3*time.Second), "42", "claim")
	seedRefTouched(t, runsDir, "3333333333333333dddddddddddddddd", 4, base.Add(2*time.Hour+3*time.Second), "43", "update")

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	counts, err := db.ProviderMutationCounts(StatsRequest{})
	if err != nil {
		t.Fatalf("ProviderMutationCounts: %v", err)
	}
	if len(counts) != 2 {
		t.Fatalf("len(counts) = %d, want 2 (claim, update)", len(counts))
	}
	if counts[0].Operation != "claim" || counts[0].Count != 2 {
		t.Fatalf("top mutation count = %#v", counts[0])
	}
}

// TestAggregateQueriesRedactCanary verifies query results carry no
// redacted-secret material — the plumbing already redacts at ingest (#22),
// this proves the aggregate query layer doesn't reintroduce a leak by
// surfacing a field that skipped that pass.
func TestAggregateQueriesRedactCanary(t *testing.T) {
	const canary = "ghp_0123456789abcdefghijklmnopqrstuvwxyz" // realistic ghp_+36 (journal net threshold, #117)
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dir := filepath.Join(runsDir, fixtureRunID)
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), minimalRunYAML(fixtureRunID, fixtureStart))
	events := strings.Join([]string{
		eventLine(1, fixtureStart, `"type":"run.started"`),
		eventLine(2, fixtureStart.Add(time.Second), `"type":"stage.started","stage":"s","attempt":1`),
		eventLine(3, fixtureStart.Add(2*time.Second), `"type":"error","stage":"s","attempt":1,"error":{"code":"harness.failure","message":"leaked `+canary+`"}`),
		eventLine(4, fixtureStart.Add(3*time.Second), `"type":"stage.finished","stage":"s","attempt":1,"status":"failure"`),
		eventLine(5, fixtureStart.Add(4*time.Second), `"type":"run.finished","status":"failed"`),
	}, "\n") + "\n"
	mustWriteFile(t, filepath.Join(dir, fileEvents), events)

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	errs, err := db.Errors(ErrorsRequest{})
	if err != nil || len(errs) != 1 {
		t.Fatalf("Errors: %v, %#v", err, errs)
	}
	if strings.Contains(errs[0].Message, canary) {
		t.Fatalf("canary leaked into Errors() result: %q", errs[0].Message)
	}

	sigs, err := db.TopErrorSignatures(StatsRequest{}, 10)
	if err != nil || len(sigs) != 1 {
		t.Fatalf("TopErrorSignatures: %v, %#v", err, sigs)
	}
	if strings.Contains(sigs[0].Code, canary) || strings.Contains(sigs[0].ExampleRunID, canary) {
		t.Fatalf("canary leaked into TopErrorSignatures() result: %#v", sigs[0])
	}
}
