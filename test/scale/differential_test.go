package main

import (
	"context"
	"fmt"
	"path/filepath"
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
			wantIDs := referenceRunIDs(t, ctx, service, options)
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

// TestCutoverFallsBackRatherThanRefusing pins that turning the flag on cannot
// REMOVE an answer the portal can get today.
//
// A stage filter is outside the closed set, so the read model refuses it. The
// service must fall through to the journal-derived path rather than surface the
// refusal: the cutover is an optimization, and an optimization that makes a
// working query stop working is a regression.
func TestCutoverFallsBackRatherThanRefusing(t *testing.T) {
	ctx := context.Background()
	gen, service, store := differentialFixture(t, 40)

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

	options := readservice.RunListOptions{Stage: instancefixture.StageName(0), Limit: 50}
	want := referenceRunIDs(t, ctx, service, options)
	got := referenceRunIDs(t, ctx, cutover, options)
	compareIDs(t, want, got)
	if len(got) == 0 {
		t.Error("the stage-filtered query returned nothing on both paths; the fixture does not exercise the fallback")
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
		Gaggle:   reference.Gaggle,
		Workflow: reference.Workflow,
		Phase:    reference.Phase,
		Since:    reference.Since,
		Until:    reference.Until,
		Limit:    200,
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
