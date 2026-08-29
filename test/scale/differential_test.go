package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instancefixture"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readprobe"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// The differential oracle (§14.7).
//
// §17 makes this the gate on Wave 2's cutover: "the old reconcile path is
// deleted only after the oracle is green with it disabled". The property under
// test is NO SILENT OMISSION — a read either includes everything matching its
// filters or reports that it cannot establish that, "verified by differential
// testing against the authoritative source, not by inspection".
//
// The reference is the journal-derived path that serves the portal today. It is
// slow and it is correct, which is exactly what a reference should be. If the
// read model and the reference disagree about which runs match a filter, the
// read model is wrong — and finding that here is the entire point of building
// the reference comparison before the cutover rather than after.

// TestReadModelMatchesTheJournalDerivedReference compares every supported filter
// combination against the journal-derived path, page by page.
func TestReadModelMatchesTheJournalDerivedReference(t *testing.T) {
	ctx := context.Background()
	gen, service, store := differentialFixture(t, 180)

	for _, combination := range readmodel.SupportedCombinations() {
		options, ok := optionsFor(combination.Dims, gen)
		if !ok {
			// A combination whose dimensions this fixture cannot express is not
			// silently skipped — that would let a gap hide.
			t.Errorf("fixture cannot express combination {%s}; the oracle would not cover it",
				readmodel.Key(combination.Dims))
			continue
		}
		t.Run(readmodel.Key(combination.Dims), func(t *testing.T) {
			var wantIDs []string
			if !options.OrderByActivity {
				wantIDs = referenceRunIDs(t, ctx, service, options)
			} else {
				// The activity axis has NO journal-derived implementation, and
				// that is deliberate: the reference orders by start, and sorting
				// its candidates to answer this would mean sorting all of them —
				// the unbounded shape the read model exists to remove. The service
				// refuses it rather than serving it wrongly (#1777).
				//
				// So the oracle supplies its own reference instead of skipping.
				// referenceActivityOrder pages the reference on its own axis to get
				// every run, then applies the window and the ordering in plain Go.
				// That is still an INDEPENDENT implementation — it shares no code
				// with the read model's SQL — which is the property that makes a
				// differential test worth running.
				wantIDs = referenceActivityOrder(t, ctx, service, options)
			}
			gotIDs := readModelRunIDs(t, ctx, store, options)
			compareIDs(t, wantIDs, gotIDs)
		})
	}
}

// TestReadModelMatchesTheReferenceAcrossPages pins that the agreement survives
// pagination, which is where a cursor predicate error shows up rather than in a
// single page.
func TestReadModelMatchesTheReferenceAcrossPages(t *testing.T) {
	ctx := context.Background()
	gen, service, store := differentialFixture(t, 180)
	_ = gen

	const pageSize = 20
	want := referenceRunIDs(t, ctx, service, readservice.RunListOptions{Limit: 200})

	var got []string
	options := readmodel.ListOptions{Limit: pageSize}
	for page := 0; ; page++ {
		result, err := store.ListRuns(ctx, options)
		if err != nil {
			t.Fatalf("read model page %d: %v", page, err)
		}
		for _, run := range result.Runs {
			got = append(got, run.RunID)
		}
		if !result.HasMore {
			break
		}
		options.Cursor = result.Next
		if page > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
	compareIDs(t, want, got)
}

// TestCutoverServesTheSameAnswersWithZeroJournalOpens is the cutover's own gate.
//
// The flag must change HOW a page is answered, never WHAT it answers. This runs
// the same readservice.ListRuns request with the read-model path off and on, and
// requires identical results — then confirms the difference is only in the work.
func TestCutoverServesTheSameAnswersWithZeroJournalOpens(t *testing.T) {
	ctx := context.Background()
	gen, service, store := differentialFixture(t, 140)

	// Attach the read model and enable the cutover on a second service, so both
	// paths are live against the same corpus at once.
	cutover, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		Telemetry:   nil,
		ReadModel:   store,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("construct cutover service: %v", err)
	}
	cutover.EnableReadModelReads()

	for _, options := range []readservice.RunListOptions{
		{Limit: 50},
		{Gaggle: instancefixture.GaggleName(0), Limit: 50},
		{Gaggle: instancefixture.GaggleName(0), Workflow: instancefixture.WorkflowName(0), Limit: 50},
		{Phase: journal.PhaseCompleted, Limit: 50},
	} {
		name := fmt.Sprintf("gaggle=%q workflow=%q phase=%q", options.Gaggle, options.Workflow, options.Phase)
		t.Run(name, func(t *testing.T) {
			want := referenceRunIDs(t, ctx, service, options)

			readprobe.Enable()
			before := readprobe.Take()
			got := referenceRunIDs(t, ctx, cutover, options)
			work := readprobe.Take().Sub(before)
			readprobe.Disable()

			compareIDs(t, want, got)
			if work.JournalOpens != 0 || work.ActiveScanOpens != 0 {
				t.Errorf("the cutover path opened %d journals (%d via the active scan); the whole "+
					"point of the cutover is that it opens none",
					work.JournalOpens, work.ActiveScanOpens)
			}
		})
	}
}

// TestCutoverRefusesUnsupportedFilterWithoutJournalFallback pins that the
// read-model cutover never turns an unsupported filter into a journal scan.
func TestCutoverRefusesUnsupportedFilterWithoutJournalFallback(t *testing.T) {
	ctx := context.Background()
	gen, _, store := differentialFixture(t, 40)

	cutover, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		Telemetry:   nil,
		ReadModel:   store,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("construct cutover service: %v", err)
	}
	cutover.EnableReadModelReads()

	options := readservice.RunListOptions{
		Workflow: instancefixture.WorkflowName(0),
		Stage:    instancefixture.StageName(0),
		Limit:    50,
	}

	readprobe.Enable()
	t.Cleanup(readprobe.Disable)
	before := readprobe.Take()
	_, err = cutover.ListRuns(ctx, options)
	work := readprobe.Take().Sub(before)

	var unsupported *readmodel.UnsupportedCombinationError
	if !errors.As(err, &unsupported) {
		t.Fatalf("ListRuns() error = %v, want typed unsupported-filter refusal", err)
	}
	if got := readmodel.Key(unsupported.Dims); got != "workflow+stage" {
		t.Errorf("refused dimensions = %q, want workflow+stage", got)
	}
	if work.JournalOpens != 0 || work.ActiveScanOpens != 0 {
		t.Errorf("refused query opened %d journals (%d via the active scan), want zero",
			work.JournalOpens, work.ActiveScanOpens)
	}
}

// TestReadModelListDoesNoJournalWork pins §14.2 end to end, against a real
// generated corpus rather than hand-written rows.
func TestReadModelListDoesNoJournalWork(t *testing.T) {
	ctx := context.Background()
	_, service, store := differentialFixture(t, 120)

	// The reference path, for contrast: this is what the portal pays today.
	readprobe.Enable()
	t.Cleanup(readprobe.Disable)
	before := readprobe.Take()
	if _, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50}); err != nil {
		t.Fatalf("reference list: %v", err)
	}
	reference := readprobe.Take().Sub(before)

	before = readprobe.Take()
	if _, err := store.ListRuns(ctx, readmodel.ListOptions{Limit: 50}); err != nil {
		t.Fatalf("read model list: %v", err)
	}
	model := readprobe.Take().Sub(before)

	t.Logf("journal-derived path: %d journal opens; read model: %d",
		reference.JournalOpens, model.JournalOpens)

	if reference.JournalOpens == 0 {
		t.Fatal("the reference path opened no journals; the comparison proves nothing")
	}
	if model.JournalOpens != 0 || model.ActiveScanOpens != 0 {
		t.Errorf("the read model opened %d journals (%d via the active scan); §14.2's bound is zero",
			model.JournalOpens, model.ActiveScanOpens)
	}
}

// differentialFixture builds a corpus, the journal-derived read service, and a
// read model populated from the same journals.
func differentialFixture(t *testing.T, runs int) (GenerateResult, *readservice.Local, *readmodel.Store) {
	t.Helper()
	spec := correctnessSpec(t.TempDir())
	spec.Runs = runs
	spec.EventsPerRun = 6
	spec.SpansPerRun = 0
	spec.SpanBytes = 0
	spec.ExtraSpanFraction = 0
	spec.OversizedRuns = 0
	spec.OrphanDirs = 3
	spec.SchedulerEvents = 40
	spec.GiantSchedulerRecords = 0

	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := rebuildAllRoots(gen); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}
	db, err := rollup.Open(gen.Layout.TelemetryDB())
	if err != nil {
		t.Fatalf("open rollup: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("construct read service: %v", err)
	}

	store, err := readmodel.Open(filepath.Join(gen.Layout.Root, readmodel.FileName))
	if err != nil {
		t.Fatalf("open read model: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Measurement, attached BEFORE the build (#1782). The population flags come
	// from the telemetry rollup and from no journal event, so a store built
	// without a source projects every run with all four flags at zero -- and the
	// oracle then compares a reference that finds 90 runs against a read model
	// that finds none.
	//
	// That is exactly what happened on the first run of this fixture, and it is
	// the same omission the daemon, the rebuild command, and the standalone
	// dashboard each had to be given a source to avoid. Four places, one
	// requirement: whoever opens a store that will be projected into must attach
	// one.
	store.WithMeasurement(readservice.NewTelemetryMeasurement(db))

	roots, err := gen.Layout.RunDirs()
	if err != nil {
		t.Fatalf("run roots: %v", err)
	}
	result, err := store.BuildFromJournals(context.Background(), roots)
	if err != nil {
		t.Fatalf("build read model: %v", err)
	}
	if result.Projected != runs {
		t.Fatalf("read model projected %d runs, want %d — the oracle would compare against an "+
			"incomplete model and pass for the wrong reason", result.Projected, runs)
	}
	return gen, service, store
}

// optionsFor builds a concrete filter for a combination, using values the
// fixture actually contains — a filter matching nothing would make the
// comparison vacuous.
func optionsFor(dims []readmodel.Dim, gen GenerateResult) (readservice.RunListOptions, bool) {
	options := readservice.RunListOptions{Limit: 200}
	for _, dim := range dims {
		switch dim {
		case readmodel.DimGaggle:
			options.Gaggle = instancefixture.GaggleName(0)
		case readmodel.DimWorkflow:
			options.Workflow = instancefixture.WorkflowName(0)
		case readmodel.DimPhase:
			options.Phase = journal.PhaseCompleted
		case readmodel.DimSince:
			options.Since = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		case readmodel.DimUntil:
			options.Until = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		case readmodel.DimStage:
			options.Stage = instancefixture.StageName(0)
		case readmodel.DimOutcome:
			options.Outcome = readservice.OutcomeSuccess
		case readmodel.DimPopulation:
			options.StagePopulation = readservice.StagePopulationRetryWaste
		case readmodel.DimActivity:
			options.OrderByActivity = true
		default:
			return readservice.RunListOptions{}, false
		}
	}
	_ = gen
	return options, true
}

// referenceRunIDs pages the journal-derived path to exhaustion.
func referenceRunIDs(t *testing.T, ctx context.Context, service *readservice.Local, options readservice.RunListOptions) []string {
	t.Helper()
	var out []string
	for page := 0; ; page++ {
		result, err := service.ListRuns(ctx, options)
		if err != nil {
			t.Fatalf("reference page %d: %v", page, err)
		}
		for _, run := range result.Runs {
			out = append(out, run.ID)
		}
		if result.NextCursor == "" {
			return out
		}
		options.Cursor = result.NextCursor
		if page > 50 {
			t.Fatal("reference pagination did not terminate")
		}
	}
}

// readModelRunIDs pages the read model with the equivalent filter.
func readModelRunIDs(t *testing.T, ctx context.Context, store *readmodel.Store, reference readservice.RunListOptions) []string {
	t.Helper()
	options := readmodel.ListOptions{
		Gaggle:     reference.Gaggle,
		Workflow:   reference.Workflow,
		Phase:      reference.Phase,
		Since:      reference.Since,
		Until:      reference.Until,
		Limit:      200,
		Stage:      reference.Stage,
		Outcome:    readmodel.Outcome(reference.Outcome),
		Population: readmodel.Population(reference.StagePopulation),
	}
	if reference.OrderByActivity {
		options.OrderBy = readmodel.OrderLastActivity
	}
	var out []string
	for page := 0; ; page++ {
		result, err := store.ListRuns(ctx, options)
		if err != nil {
			t.Fatalf("read model page %d: %v", page, err)
		}
		for _, run := range result.Runs {
			out = append(out, run.RunID)
		}
		if !result.HasMore {
			return out
		}
		options.Cursor = result.Next
		if page > 50 {
			t.Fatal("read model pagination did not terminate")
		}
	}
}

// compareIDs reports the exact difference rather than only that one exists.
//
// "The two lists differ" is not actionable; which runs were dropped or added is.
// Silent omission is the failure mode under test, so a missing run must be named.
func compareIDs(t *testing.T, want, got []string) {
	t.Helper()
	wantSet := make(map[string]bool, len(want))
	for _, id := range want {
		wantSet[id] = true
	}
	gotSet := make(map[string]bool, len(got))
	for _, id := range got {
		gotSet[id] = true
	}

	var missing, extra []string
	for _, id := range want {
		if !gotSet[id] {
			missing = append(missing, id)
		}
	}
	for _, id := range got {
		if !wantSet[id] {
			extra = append(extra, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("read model OMITTED %d run(s) the journal-derived reference returned: %s\n"+
			"This is the silent-omission failure §14.7 exists to catch.",
			len(missing), sample(missing))
	}
	if len(extra) > 0 {
		t.Errorf("read model returned %d run(s) the reference did not: %s",
			len(extra), sample(extra))
	}
	if len(want) != len(got) {
		t.Errorf("reference returned %d runs, read model %d", len(want), len(got))
	}
	// Order must match too: both claim newest-first, and a list that agrees on
	// membership but not order is still a different page to a user.
	if len(missing) == 0 && len(extra) == 0 {
		for i := range want {
			if want[i] != got[i] {
				t.Errorf("ordering diverged at position %d: reference %s, read model %s",
					i, want[i], got[i])
				break
			}
		}
	}
}

func sample(ids []string) string {
	if len(ids) > 5 {
		return fmt.Sprintf("%v (and %d more)", ids[:5], len(ids)-5)
	}
	return fmt.Sprintf("%v", ids)
}

// TestAggregateMatchesTheJournalPathWithZeroJournalOpens is the differential for
// the Workflows page (#1891).
//
// The journal-derived path for latestPerWorkflow is the most expensive read in
// the portal: a window function over all history to pick each workflow's newest
// run, then a BACKWARDS WALK per workflow that opens the newest run's journal
// and keeps paging further back until it finds a terminal one. The aggregate
// answers the same page from one indexed query.
//
// Both halves are asserted, because either alone would be misleading: the same
// run IDs with the same journal cost is no improvement, and zero journal opens
// with different answers is a bug.
func TestAggregateMatchesTheJournalPathWithZeroJournalOpens(t *testing.T) {
	ctx := context.Background()
	gen, service, store := differentialFixture(t, 140)

	cutover, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		ReadModel:   store,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("construct cutover service: %v", err)
	}
	cutover.EnableReadModelReads()

	for _, options := range []readservice.RunListOptions{
		{LatestPerWorkflow: true},
		{LatestPerWorkflow: true, Gaggle: instancefixture.GaggleName(0)},
		{
			LatestPerWorkflow: true,
			Gaggle:            instancefixture.GaggleName(0),
			Workflow:          instancefixture.WorkflowName(0),
		},
	} {
		name := fmt.Sprintf("gaggle=%q workflow=%q", options.Gaggle, options.Workflow)
		t.Run(name, func(t *testing.T) {
			readprobe.Enable()
			before := readprobe.Take()
			want, err := service.ListRuns(ctx, options)
			journalWork := readprobe.Take().Sub(before)
			readprobe.Disable()
			if err != nil {
				t.Fatalf("journal path: %v", err)
			}

			readprobe.Enable()
			before = readprobe.Take()
			got, err := cutover.ListRuns(ctx, options)
			aggregateWork := readprobe.Take().Sub(before)
			readprobe.Disable()
			if err != nil {
				t.Fatalf("aggregate path: %v", err)
			}

			t.Logf("journal path: %d journal opens; aggregate: %d",
				journalWork.JournalOpens, aggregateWork.JournalOpens)

			compareIDs(t, runIDsOf(want), runIDsOf(got))
			compareActivity(t, want.WorkflowActivity, got.WorkflowActivity)

			if aggregateWork.JournalOpens != 0 || aggregateWork.ActiveScanOpens != 0 {
				t.Errorf("the aggregate opened %d journals (%d via the active scan); §14.4 requires "+
					"a workflow page to issue one aggregate request and zero per-workflow reads",
					aggregateWork.JournalOpens, aggregateWork.ActiveScanOpens)
			}
			// The fixture is small enough that the journal path is cheap, so this
			// asserts the DIRECTION rather than a ratio: if the reference did no
			// journal work either, the comparison proves nothing and the test is
			// silently vacuous.
			if journalWork.JournalOpens == 0 {
				t.Errorf("the reference path opened no journals, so this test cannot show the " +
					"aggregate removed any; the fixture no longer exercises the backwards walk")
			}
		})
	}
}

// TestAggregateReportsTheLastOutcomeWhileARunIsInFlight pins the distinction the
// Workflows page is built on, end to end through the service.
//
// A workflow with a finished run and a newer running one must report the
// FINISHED run as its latest outcome, and the running one in workflowActivity.
// Reporting the running run as the outcome blanks the result column exactly when
// someone is watching; omitting the activity hides that anything is happening.
func TestAggregateReportsTheLastOutcomeWhileARunIsInFlight(t *testing.T) {
	ctx := context.Background()
	gen, service, store := differentialFixture(t, 140)

	cutover, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		ReadModel:   store,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("construct cutover service: %v", err)
	}
	cutover.EnableReadModelReads()

	options := readservice.RunListOptions{LatestPerWorkflow: true}
	want, err := service.ListRuns(ctx, options)
	if err != nil {
		t.Fatalf("journal path: %v", err)
	}
	got, err := cutover.ListRuns(ctx, options)
	if err != nil {
		t.Fatalf("aggregate path: %v", err)
	}

	// Every returned run is terminal on BOTH paths — that is what "latest
	// outcome" means, and the aggregate must not relax it.
	for _, list := range []struct {
		label string
		runs  []readservice.RunSummary
	}{{"journal", want.Runs}, {"aggregate", got.Runs}} {
		for _, run := range list.runs {
			if !run.Terminal {
				t.Errorf("%s path returned non-terminal run %s (phase %s) as a latest OUTCOME",
					list.label, run.ID, run.Phase)
			}
		}
	}
	if len(got.Runs) == 0 {
		t.Fatal("the aggregate returned no outcomes; the fixture has terminal runs")
	}
}

// runIDsOf extracts run IDs in returned order.
func runIDsOf(list readservice.RunList) []string {
	out := make([]string, 0, len(list.Runs))
	for _, run := range list.Runs {
		out = append(out, run.ID)
	}
	return out
}

// compareActivity pins that both paths report the same per-workflow active
// counts.
//
// The counts come from genuinely different mechanisms — a sampler over the
// filesystem on one path, an indexed aggregate over read.db on the other — so
// agreement here is evidence the projection tracks reality rather than a
// tautology.
func compareActivity(t *testing.T, want, got []readservice.WorkflowRunActivity) {
	t.Helper()
	index := func(items []readservice.WorkflowRunActivity) map[string]int {
		out := make(map[string]int, len(items))
		for _, item := range items {
			out[item.Gaggle+"/"+item.Workflow] = item.ActiveRuns
		}
		return out
	}
	wantIndex, gotIndex := index(want), index(got)
	for key, wantCount := range wantIndex {
		if gotCount, ok := gotIndex[key]; !ok || gotCount != wantCount {
			t.Errorf("workflow activity for %s: aggregate reports %d (present=%v), journal path "+
				"reports %d", key, gotCount, ok, wantCount)
		}
	}
	for key, gotCount := range gotIndex {
		if _, ok := wantIndex[key]; !ok {
			t.Errorf("aggregate reports activity for %s (%d runs) that the journal path does not",
				key, gotCount)
		}
	}
}

// referenceActivityOrder computes the activity-ordered answer without the read
// model, by fetching every run through the journal-derived path and doing the
// window and the sort in Go.
//
// This is the oracle's own implementation, written deliberately naively: it
// reads everything and sorts in memory, which is exactly the O(all runs) shape
// production must not have. That is fine here and is the point — a reference is
// allowed to be slow, and one that shared the optimised path's code would prove
// nothing.
func referenceActivityOrder(
	t *testing.T,
	ctx context.Context,
	service *readservice.Local,
	options readservice.RunListOptions,
) []string {
	t.Helper()

	// Fetch on the axis the reference CAN serve, unfiltered by the window: the
	// window applies to last activity, and filtering it here by started_at would
	// drop exactly the runs this axis exists to surface.
	all := options
	all.OrderByActivity = false
	all.Since = time.Time{}
	all.Until = time.Time{}
	all.Limit = 200

	var summaries []readservice.RunSummary
	for page := 0; ; page++ {
		result, err := service.ListRuns(ctx, all)
		if err != nil {
			t.Fatalf("reference activity page %d: %v", page, err)
		}
		summaries = append(summaries, result.Runs...)
		if result.NextCursor == "" {
			break
		}
		all.Cursor = result.NextCursor
		if page > 50 {
			t.Fatal("reference pagination did not terminate")
		}
	}

	kept := make([]readservice.RunSummary, 0, len(summaries))
	for _, run := range summaries {
		if !options.Since.IsZero() && run.LastActivityAt.Before(options.Since) {
			continue
		}
		if !options.Until.IsZero() && run.LastActivityAt.After(options.Until) {
			continue
		}
		kept = append(kept, run)
	}

	// Descending activity, ascending id — the same total order the SQL declares,
	// stated here independently so a disagreement is a real finding rather than
	// two copies of one mistake.
	sort.Slice(kept, func(i, j int) bool {
		if !kept[i].LastActivityAt.Equal(kept[j].LastActivityAt) {
			return kept[i].LastActivityAt.After(kept[j].LastActivityAt)
		}
		return kept[i].ID < kept[j].ID
	})

	out := make([]string, 0, len(kept))
	for _, run := range kept {
		out = append(out, run.ID)
	}
	return out
}
