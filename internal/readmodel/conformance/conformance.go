// Package conformance is the backend-neutral contract for a read-model store
// (#1921, design §13.2).
//
// # What it is
//
// An executable statement of what a read-model backend must MEAN, written
// without reference to SQLite. Any implementation of readmodel.Backend can be
// handed to Run and either satisfies the contract or does not.
//
// # Why it exists before there is a second backend
//
// Not to make a swap easy — a swap is the hosted wave's problem (#652). It
// exists because the semantics are subtle in ways that are invisible when you
// only ever run one implementation, and every one of them was decided during
// Wave 2 for a reason that is easy to lose:
//
//   - a guarded acknowledgement looks like a normal upsert until a replay
//     arrives out of order;
//   - an opaque epoch looks like a version number until someone compares two
//     with `<`;
//   - a serialized commit order looks automatic until two writers exist;
//   - a closed combination set looks like validation until an unlisted filter
//     is answered slowly instead of refused.
//
// Each of those is a behaviour a reasonable implementation gets wrong while
// passing every test that only checks the happy path. That is what this package
// is for.
//
// # What it deliberately does not cover
//
// Durability, crash recovery, and concurrency under real contention. Those are
// properties of an operational deployment, and asserting them here would make
// the contract untestable without the second database this issue explicitly
// does not require.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// Factory constructs a fresh, empty backend for one contract case.
//
// It returns a NEW store each call — several cases depend on two independent
// stores existing at once (the epoch case cannot be written otherwise), and a
// factory that returned a shared handle would make them silently vacuous.
type Factory func(t *testing.T) readmodel.Backend

// Run executes the whole contract against a backend.
func Run(t *testing.T, newBackend Factory) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, Factory)
	}{
		{"GuardedAcknowledgement", guardedAcknowledgement},
		{"ReplayIsIdempotent", replayIsIdempotent},
		{"EpochsAreOpaqueAndDistinct", epochsAreOpaqueAndDistinct},
		{"ChangeOrderIsSerializedAndDense", changeOrderIsSerializedAndDense},
		{"ChangeSeqIsNeverReused", changeSeqIsNeverReused},
		{"RetentionFloorOnlyAdvances", retentionFloorOnlyAdvances},
		{"ClosedSetIsRefusedNotAnswered", closedSetIsRefusedNotAnswered},
		{"ClosedSetIsFullyServed", closedSetIsFullyServed},
		{"PagesAreBoundedAndOrdered", pagesAreBoundedAndOrdered},
		{"FreshnessIsSourceRelative", freshnessIsSourceRelative},
		{"AbsentRunIsAnAnswerNotAnError", absentRunIsAnAnswerNotAnError},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) { testCase.run(t, newBackend) })
	}
}

// guardedAcknowledgement: a projection at or behind the stored source position
// must not overwrite it.
//
// This is the single most important property in the contract. A read model is
// rebuilt, resumed, and repaired, so the same run is projected repeatedly and
// NOT always in order — a repair sweep re-reads an old journal while the
// projector is ahead of it. A blind upsert silently rolls a run backwards, and
// the portal shows a completed run as running again with no error anywhere.
func guardedAcknowledgement(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)

	runID := runID(1)
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	finished := at.Add(time.Minute)

	ahead := readmodel.Projection{Run: readmodel.RunRow{
		RunID: runID, Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: at, FinishedAt: &finished, LastSeq: 100,
	}}
	if err := store.UpsertRun(ctx, ahead); err != nil {
		t.Fatalf("project at seq 100: %v", err)
	}

	// A stale projection: same run, EARLIER source position, and a phase that
	// would visibly regress the run if it landed.
	behind := readmodel.Projection{Run: readmodel.RunRow{
		RunID: runID, Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseRunning, Terminal: false,
		StartedAt: at, LastSeq: 40,
	}}
	if err := store.UpsertRun(ctx, behind); err != nil {
		t.Fatalf("a stale projection must be ACCEPTED and ignored, not rejected: %v", err)
	}

	got, ok, err := store.GetRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.LastSeq != 100 || got.Phase != journal.PhaseCompleted {
		t.Errorf("a projection from source position 40 overwrote one from 100: "+
			"phase=%s last_seq=%d, want completed/100. A rebuild or repair racing the "+
			"projector would roll finished runs back to running.",
			got.Phase, got.LastSeq)
	}
}

// replayIsIdempotent: projecting the same position twice changes nothing and
// emits no second change.
//
// Resumability depends on it. A build interrupted halfway and restarted
// re-projects everything it already did; if each replay emitted a change, every
// live client would see a burst of transitions that did not happen.
func replayIsIdempotent(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)

	projection := completedRun(runID(2), "alpha", "wf", 7)
	for i := 0; i < 3; i++ {
		if err := store.UpsertRun(ctx, projection); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}

	changes, err := store.Changes(ctx, 0, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) != 1 {
		t.Errorf("three identical projections emitted %d changes, want 1; a resumed build "+
			"would replay its whole prefix as live transitions", len(changes))
	}
}

// epochsAreOpaqueAndDistinct: two independently created stores must not share
// an epoch, and the epoch must carry no ordering meaning.
//
// §4.2: the epoch exists so a client holding a cursor from a REBUILT store is
// told `epoch_changed` rather than waiting forever for a sequence that will
// never arrive. That only works if a fresh store never reuses the previous
// epoch. The contract checks distinctness, and deliberately does NOT check any
// ordering relationship — an implementation whose epochs happened to sort would
// invite exactly the comparison §4.2 forbids.
func epochsAreOpaqueAndDistinct(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	seen := make(map[string]struct{})
	for i := 0; i < 8; i++ {
		state, err := newBackend(t).State(ctx)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		if state.Epoch == "" {
			t.Fatal("a store reported an empty epoch; a client could not distinguish it from " +
				"an unset cursor field")
		}
		if _, dup := seen[state.Epoch]; dup {
			t.Fatalf("two independently created stores share epoch %q; a client holding a "+
				"cursor from the old one would be told to continue against the new one, "+
				"and would wait forever for a sequence that will never arrive", state.Epoch)
		}
		seen[state.Epoch] = struct{}{}
	}
}

// changeOrderIsSerializedAndDense: changes are returned in strictly increasing
// commit order, and Changes(after) resumes exactly where the caller left off.
//
// "Serialized" is the property live updates rest on (§8.2). A client that has
// seen position N must be able to ask for everything after N and miss nothing.
func changeOrderIsSerializedAndDense(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)

	const runs = 12
	for i := 0; i < runs; i++ {
		if err := store.UpsertRun(ctx, completedRun(runID(100+i), "alpha", "wf", uint64(i+1))); err != nil {
			t.Fatalf("project %d: %v", i, err)
		}
	}

	all, err := store.Changes(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(all) != runs {
		t.Fatalf("got %d changes for %d distinct runs, want %d", len(all), runs, runs)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("change %d has seq %d, not greater than the previous %d; commit order "+
				"is not serialized and a resuming client would skip or repeat",
				i, all[i].Seq, all[i-1].Seq)
		}
	}

	// Walking in pages from each position must reconstruct the same sequence.
	// This is the property a live client actually depends on, and it is not
	// implied by monotonicity alone.
	var walked []readmodel.Change
	after := uint64(0)
	for {
		page, err := store.Changes(ctx, after, 5)
		if err != nil {
			t.Fatalf("changes after %d: %v", after, err)
		}
		if len(page) == 0 {
			break
		}
		walked = append(walked, page...)
		after = page[len(page)-1].Seq
	}
	if len(walked) != len(all) {
		t.Errorf("paging through the feed yielded %d changes but reading it whole yielded %d; "+
			"a resuming client would not see the same history as a fresh one",
			len(walked), len(all))
	}
	for i := range walked {
		if i < len(all) && walked[i].Seq != all[i].Seq {
			t.Errorf("paged change %d has seq %d, whole-feed has %d", i, walked[i].Seq, all[i].Seq)
		}
	}

	latest, err := store.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("latest change seq: %v", err)
	}
	if len(all) > 0 && latest != all[len(all)-1].Seq {
		t.Errorf("LatestChangeSeq reported %d but the newest change is %d; a client would "+
			"subscribe from a position that does not exist", latest, all[len(all)-1].Seq)
	}
}

// changeSeqIsNeverReused: positions freed by pruning must not be handed out
// again.
//
// A reused position gives two different transitions the same cursor. A client
// that resumes at that position gets whichever one is there now, with no way to
// detect the substitution — the cursor still parses, still validates, and still
// points into the feed.
func changeSeqIsNeverReused(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)

	for i := 0; i < 6; i++ {
		if err := store.UpsertRun(ctx, completedRun(runID(200+i), "alpha", "wf", uint64(i+1))); err != nil {
			t.Fatalf("project %d: %v", i, err)
		}
	}
	before, err := store.Changes(ctx, 0, 100)
	if err != nil || len(before) == 0 {
		t.Fatalf("changes: %d rows, err=%v", len(before), err)
	}
	highest := before[len(before)-1].Seq

	// Prune everything, then write more. The new positions must be above the
	// highest ever issued, not above the highest still present.
	if _, err := store.PruneChanges(ctx, highest+1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := store.UpsertRun(ctx, completedRun(runID(300+i), "alpha", "wf", uint64(i+1))); err != nil {
			t.Fatalf("project after prune %d: %v", i, err)
		}
	}
	after, err := store.Changes(ctx, 0, 100)
	if err != nil {
		t.Fatalf("changes after prune: %v", err)
	}
	for _, change := range after {
		if change.Seq <= highest {
			t.Errorf("a change written after pruning got seq %d, at or below the pruned "+
				"high-water mark %d; two different transitions now share a cursor position",
				change.Seq, highest)
		}
	}
}

// retentionFloorOnlyAdvances: pruning raises the floor, and a smaller prune
// never lowers it.
//
// The floor is what turns "your cursor is too old" into a NAMED condition
// (§8.2's feed_truncated) instead of an empty result that looks like "nothing
// has happened". A floor that could move backwards would let a client resume
// below it and silently miss the changes that were already dropped.
func retentionFloorOnlyAdvances(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)

	for i := 0; i < 10; i++ {
		if err := store.UpsertRun(ctx, completedRun(runID(400+i), "alpha", "wf", uint64(i+1))); err != nil {
			t.Fatalf("project %d: %v", i, err)
		}
	}
	changes, err := store.Changes(ctx, 0, 100)
	if err != nil || len(changes) < 6 {
		t.Fatalf("need at least 6 changes, got %d (err=%v)", len(changes), err)
	}

	if _, err := store.PruneChanges(ctx, changes[5].Seq); err != nil {
		t.Fatalf("prune: %v", err)
	}
	high, err := store.State(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	// A prune at a LOWER position must not lower the floor.
	if _, err := store.PruneChanges(ctx, changes[1].Seq); err != nil {
		t.Fatalf("second prune: %v", err)
	}
	low, err := store.State(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if low.MinChangeSeq < high.MinChangeSeq {
		t.Errorf("the retention floor moved backwards, %d -> %d; a client could resume below "+
			"it and silently miss changes that were already dropped",
			high.MinChangeSeq, low.MinChangeSeq)
	}
}

// closedSetIsRefusedNotAnswered: an unlisted filter combination must produce a
// typed refusal naming alternatives — not a slow success.
//
// §5.7's whole argument is that a bound cannot come from a query plan, because
// a plan does not reveal residual predicates. It comes from a declared set. An
// implementation that answers an unlisted combination anyway has replaced a
// guarantee with a hope, and nothing observable changes until it is slow in
// production.
func closedSetIsRefusedNotAnswered(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)

	// A workflow filter WITHOUT a gaggle. The set pairs workflow only with
	// gaggle, because the ordering indexes lead with gaggle and a bare workflow
	// predicate would be residual against every one of them.
	//
	// This is the sharpest available case: what a backend can still get wrong is
	// an unsupported COMBINATION of individually supported dimensions.
	_, err := store.ListRuns(ctx, readmodel.ListOptions{
		Workflow: "wf",
		Limit:    10,
	})
	if err == nil {
		t.Fatal("an unlisted filter combination was ANSWERED rather than refused; the closed " +
			"set is the only bound on rows examined, and answering makes it advisory")
	}
	var unsupported *readmodel.UnsupportedCombinationError
	if !errors.As(err, &unsupported) {
		t.Fatalf("refusal is not typed: %v (%T); a caller cannot distinguish it from a "+
			"backend failure and would retry", err, err)
	}
	if unsupported.Code() != "unsupported_filter_combination" {
		t.Errorf("refusal code = %q, want unsupported_filter_combination", unsupported.Code())
	}
	if len(unsupported.Neighbours) == 0 {
		t.Error("the refusal names no supported alternatives; these combinations are reachable " +
			"by an ordinary stale bookmark, so a bare no leaves the user nowhere to go")
	}
}

// closedSetIsFullyServed: every combination the set DECLARES must actually be
// answerable.
//
// The converse of the refusal case, and the one that rots quietly: a
// combination listed as supported but not implemented turns the set from a
// guarantee into documentation. Enumerating from SupportedCombinations rather
// than a hand-written list is what makes adding an entry without an
// implementation fail here.
func closedSetIsFullyServed(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)
	if err := store.UpsertRun(ctx, completedRun(runID(500), "alpha", "wf", 1)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, combination := range readmodel.SupportedCombinations() {
		options := optionsForDims(combination.Dims)
		options.Limit = 10
		if _, err := store.ListRuns(ctx, options); err != nil {
			t.Errorf("combination {%s} is declared supported but could not be served: %v",
				readmodel.Key(combination.Dims), err)
		}
		if combination.Index == "" {
			t.Errorf("combination {%s} declares no index; §5.7 requires that adding a "+
				"combination ships an index with it", readmodel.Key(combination.Dims))
		}
	}
}

// pagesAreBoundedAndOrdered: a page never exceeds its limit, and ordering is
// newest-first with a stable tiebreak.
//
// The tiebreak is the part that matters and the part an implementation is most
// likely to omit: without it, two runs sharing a started_at can swap places
// between requests, and a keyset cursor built on the pair either repeats a run
// forever or skips one.
func pagesAreBoundedAndOrdered(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)

	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Three runs share a timestamp — the tiebreak case.
	for i := 0; i < 9; i++ {
		projection := completedRun(runID(600+i), "alpha", "wf", uint64(i+1))
		projection.Run.StartedAt = at.Add(time.Duration(i/3) * time.Hour)
		finished := projection.Run.StartedAt.Add(time.Minute)
		projection.Run.FinishedAt = &finished
		if err := store.UpsertRun(ctx, projection); err != nil {
			t.Fatalf("project %d: %v", i, err)
		}
	}

	page, err := store.ListRuns(ctx, readmodel.ListOptions{Gaggle: "alpha", Limit: 4})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Runs) > 4 {
		t.Errorf("a page with limit 4 returned %d rows; the lookahead row used to detect "+
			"another page must never be returned", len(page.Runs))
	}
	for i := 1; i < len(page.Runs); i++ {
		prev, cur := page.Runs[i-1], page.Runs[i]
		if cur.StartedAt.After(prev.StartedAt) {
			t.Errorf("row %d started after row %d; the list is not newest-first", i, i-1)
		}
		if cur.StartedAt.Equal(prev.StartedAt) && cur.RunID <= prev.RunID {
			t.Errorf("rows %d and %d share a timestamp but run IDs are not ascending (%s, %s); "+
				"without a stable tiebreak a keyset cursor repeats or skips",
				i-1, i, prev.RunID, cur.RunID)
		}
	}

	// Paging with the returned cursor must not repeat the last row.
	if page.HasMore {
		next, err := store.ListRuns(ctx, readmodel.ListOptions{
			Gaggle: "alpha", Limit: 4, Cursor: page.Next,
		})
		if err != nil {
			t.Fatalf("second page: %v", err)
		}
		for _, run := range next.Runs {
			for _, seen := range page.Runs {
				if run.RunID == seen.RunID {
					t.Errorf("run %s appears on both pages; the cursor is not exclusive", run.RunID)
				}
			}
		}
	}
}

// freshnessIsSourceRelative: what the store reports about a run's currency must
// come from the SOURCE position it observed, never from a clock.
//
// §14.7's requirement is truthful freshness. A store that reported "projected
// at 12:04" would look current while being arbitrarily far behind the journal;
// the honest statement is which source position it has consumed, because that
// is the only quantity comparable to the source itself.
func freshnessIsSourceRelative(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)

	runIdentity := runID(700)
	if err := store.UpsertRun(ctx, completedRun(runIdentity, "alpha", "wf", 42)); err != nil {
		t.Fatalf("project: %v", err)
	}
	got, ok, err := store.GetRun(ctx, runIdentity)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.LastSeq != 42 {
		t.Errorf("the store reports source position %d for a run projected at 42; a position "+
			"it did not observe cannot be compared against the journal, which is the only "+
			"thing that makes freshness checkable", got.LastSeq)
	}

	// Advancing the source advances the reported position — the property that
	// makes it a watermark rather than a constant.
	if err := store.UpsertRun(ctx, completedRun(runIdentity, "alpha", "wf", 99)); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got, _, err = store.GetRun(ctx, runIdentity)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.LastSeq != 99 {
		t.Errorf("source position stayed at %d after projecting 99; it is not tracking the "+
			"source", got.LastSeq)
	}
}

// absentRunIsAnAnswerNotAnError: a run the store does not have must be reported
// as absent, not as a failure.
//
// Retention deletes runs on purpose. If absence were an error, a page that
// included a since-pruned run would fail entirely instead of omitting it, and
// ordinary retention would look like breakage.
func absentRunIsAnAnswerNotAnError(t *testing.T, newBackend Factory) {
	ctx := context.Background()
	store := newBackend(t)

	_, ok, err := store.GetRun(ctx, runID(999))
	if err != nil {
		t.Errorf("reading an absent run returned an error (%v); retention would be "+
			"indistinguishable from breakage", err)
	}
	if ok {
		t.Error("the store claims to have a run that was never projected")
	}
}

// completedRun builds a minimal terminal projection at a source position.
func completedRun(id, gaggle, workflow string, seq uint64) readmodel.Projection {
	startedAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Minute)
	finished := startedAt.Add(time.Minute)
	return readmodel.Projection{Run: readmodel.RunRow{
		RunID: id, Gaggle: gaggle, Workflow: workflow,
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: startedAt, FinishedAt: &finished, LastSeq: seq,
	}}
}

// optionsForDims builds a list request constraining exactly these dimensions.
//
// Values are arbitrary — the case is about whether the combination is SERVED,
// not about what it matches — but every dimension the set can declare has to be
// representable here, so a new dimension added without a mapping fails loudly
// rather than being silently skipped.
func optionsForDims(dims []readmodel.Dim) readmodel.ListOptions {
	var options readmodel.ListOptions
	for _, dim := range dims {
		switch dim {
		case readmodel.DimGaggle:
			options.Gaggle = "alpha"
		case readmodel.DimWorkflow:
			options.Workflow = "wf"
		case readmodel.DimPhase:
			options.Phase = journal.PhaseCompleted
		case readmodel.DimSince:
			options.Since = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		case readmodel.DimUntil:
			options.Until = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		case readmodel.DimStage:
			options.Stage = "build"
		case readmodel.DimOutcome:
			options.Outcome = readmodel.OutcomeSuccess
		case readmodel.DimPopulation:
			options.Population = readmodel.PopulationCostMeasured
		case readmodel.DimActivity:
			options.OrderBy = readmodel.OrderLastActivity
		default:
			panic(fmt.Sprintf("conformance: unknown dimension %q in the supported set", dim))
		}
	}
	return options
}

// runID renders a run identifier of the shape the store stores.
func runID(n int) string { return fmt.Sprintf("%032x", n) }
