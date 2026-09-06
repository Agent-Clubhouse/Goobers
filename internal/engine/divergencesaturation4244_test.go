package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// #4244: DS5 filed a live_journal_divergence on ~98% of verified runs —
// 10,220 filings over ~1,126 runs in ten days, 10,218 of the window's 13,619
// error events — because the live journal carries emission-only events
// (artifact.recorded from the stage's own journal-plane emission,
// agent.lifecycle from an agentic pod stage) that the history re-projection
// has no counterpart for: the artifact's POINTER rides the surrendered
// envelope into history and replays onto stage.finished, while the emission
// itself never reaches history at all. One unmatched live event is an
// INSERTION, so it misaligns every normative index after it and the guard
// reported a divergence on every run that recorded an artifact.
//
// The tests below cover the fix's three claims: the shape no longer diverges,
// the premise it rests on is still true, and — the one that matters most,
// since #4244 named over-broad exclusion as the risk — a GENUINE artifact
// divergence is still filed.

// TestEmissionOnlyArtifactDoesNotDiverge is the regression proper: a run that
// records both a stage-result artifact (emission-only, live side) and a
// branch ref (ref.touched, both sides) verifies clean.
func TestEmissionOnlyArtifactDoesNotDiverge(t *testing.T) {
	writer, runsDir := newLiveWriter(t)
	// A REPO-workspace stage, so the run records its branch ref — the
	// both-sides half of the shape the fix has to keep comparing.
	implement := apiv1.Task{
		Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
		Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo},
		Next: "review",
	}
	spec := crSpec("implement",
		[]apiv1.Task{implement},
		[]apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})})
	in := projectionInput("live-emission-only-artifact", spec)
	in.LiveJournal = true

	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}
	proj := executeLive(t, in, &Activities{
		Det:        det,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
		Journal:    writer,
	}, false)

	live := liveEvents(t, runsDir, in.RunID)

	// Preconditions, asserted loudly: without them a green result below would
	// mean the fixture stopped exercising the bug rather than the bug being
	// fixed.
	if !containsType(live, journal.EventRefTouched) {
		t.Fatal("fixture recorded no ref.touched: the both-sides half of the shape is missing")
	}
	projected := reprojectedEvents(t, proj)
	if !containsType(projected, journal.EventRefTouched) {
		t.Fatal("the re-projection has no ref.touched: the fixture is not exercising a both-sides event")
	}
	if countType(live, journal.EventArtifactRecorded) != countType(projected, journal.EventArtifactRecorded) {
		t.Fatal("fixture is already asymmetric in artifact.recorded before the surplus is added; the baseline below is not a baseline")
	}
	if divergence, err := DiffLiveJournal(live, proj); err != nil || divergence != "" {
		t.Fatalf("baseline must verify clean before the surplus is added: divergence=%q err=%v", divergence, err)
	}

	// The pod's journal plane emits a stage's streams straight into the LIVE
	// journal while only a POINTER rides the surrendered envelope into history
	// (cmd/goobers/dispatchexec.go, MEASURED on a live cluster). The engine's
	// own in-process harness has no pod, so the surplus is added here in the
	// exact shape and position that comment records — an artifact.recorded
	// inside the stage, with no counterpart op in the history being replayed.
	live = spliceAfter(t, live, journal.EventStageStarted, journal.Event{
		Type:  journal.EventArtifactRecorded,
		Stage: "implement",
		Name:  "cli-on-pod/stdout.log",
		Ref:   &journal.Ref{Path: "artifacts/stdout.log", Digest: "sha256:" + strings.Repeat("a", 64)},
	})

	divergence, err := DiffLiveJournal(live, proj)
	if err != nil {
		t.Fatalf("DiffLiveJournal: %v", err)
	}
	if divergence != "" {
		t.Fatalf("an emission-only artifact was filed as a divergence (#4244):\n%s", divergence)
	}
}

// TestLiveJournalMissingArtifactStillDiverges is the other direction of the
// surplus rule, and the reason it is a surplus rule rather than a type
// exclusion: history carrying an artifact the live journal does not is a
// genuine parity failure and must still be filed.
func TestLiveJournalMissingArtifactStillDiverges(t *testing.T) {
	writer, runsDir := newLiveWriter(t)
	spec := crSpec("implement",
		[]apiv1.Task{crTask("implement", "review")},
		[]apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})})
	in := projectionInput("live-artifact-dropped", spec)
	in.LiveJournal = true

	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}
	proj := executeLive(t, in, &Activities{
		Det:        det,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
		Journal:    writer,
	}, false)

	live := liveEvents(t, runsDir, in.RunID)
	if divergence, err := DiffLiveJournal(live, proj); err != nil || divergence != "" {
		t.Fatalf("baseline must verify clean before the drop: divergence=%q err=%v", divergence, err)
	}

	dropped := make([]journal.Event, 0, len(live))
	for _, ev := range live {
		if ev.Type == journal.EventArtifactRecorded {
			continue
		}
		dropped = append(dropped, ev)
	}
	if len(dropped) == len(live) {
		t.Fatal("fixture has no artifact.recorded to drop: it cannot cover this direction")
	}

	divergence, err := DiffLiveJournal(dropped, proj)
	if err != nil {
		t.Fatalf("DiffLiveJournal: %v", err)
	}
	if divergence == "" {
		t.Fatal("a live journal missing an artifact history recorded verified clean: the accommodation is forgiving the wrong direction (#4244)")
	}
}

// TestExclusionStaysNarrow pins the accommodation's type list. #4244 named an
// over-broad exclusion as the whole risk of this fix — "an over-broad
// exclusion could mask a real future divergence" — so growing this list must
// be a reviewed act, not a side effect of some other change. It also pins the
// premise: if these types stop being conformance-normative the accommodation
// is a no-op and this regression is no longer testing what it claims.
func TestExclusionStaysNarrow(t *testing.T) {
	want := map[journal.EventType]bool{
		journal.EventArtifactRecorded: true,
		journal.EventAgentLifecycle:   true,
	}
	for typ := range emissionOnlyTypes {
		if !want[typ] {
			t.Errorf("%s was added to the DS5 exclusion list: state in verify.go why history can never carry it, and pin it here (#4244)", typ)
		}
		if !(journal.Event{Type: typ}).IsConformanceNormative() {
			t.Errorf("premise moved: %s is no longer conformance-normative, so excluding it from DS5 is now a no-op — re-derive the fix", typ)
		}
	}
	for typ := range want {
		if !emissionOnlyTypes[typ] {
			t.Errorf("%s dropped out of the DS5 exclusion: #4244's saturation returns for that shape", typ)
		}
	}
	// The orchestration spine the guard exists to compare must never be in
	// the list, whatever else is.
	for _, typ := range []journal.EventType{
		journal.EventRunStarted, journal.EventRunFinished,
		journal.EventStageStarted, journal.EventStageFinished,
		journal.EventGateEvaluated, journal.EventRefTouched, journal.EventError,
	} {
		if emissionOnlyTypes[typ] {
			t.Errorf("%s was excluded from the DS5 comparison: the parity guard no longer covers it", typ)
		}
	}
}

// TestArtifactContentDivergenceStillFiles is the load-bearing counter-test:
// an artifact that DISAGREES across the two sides matches nothing, so the
// surplus rule does not forgive it and DS5 still files. This is the coverage a
// blanket type exclusion would have silently retired.
func TestArtifactContentDivergenceStillFiles(t *testing.T) {
	writer, runsDir := newLiveWriter(t)
	spec := crSpec("implement",
		[]apiv1.Task{crTask("implement", "review")},
		[]apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})})
	in := projectionInput("live-artifact-corruption", spec)
	in.LiveJournal = true

	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}
	proj := executeLive(t, in, &Activities{
		Det:        det,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
		Journal:    writer,
	}, false)

	live := liveEvents(t, runsDir, in.RunID)
	if divergence, err := DiffLiveJournal(live, proj); err != nil || divergence != "" {
		t.Fatalf("baseline must verify clean before corruption: divergence=%q err=%v", divergence, err)
	}

	// Make history claim the stage recorded a DIFFERENT artifact than the live
	// journal says it did. Name is compared for every artifact; content digest
	// is compared for all but the context manifest, whose digest conformance.go
	// deliberately excludes (isContextManifestArtifact) — and the context
	// manifest is the only artifact this in-process fixture produces, so
	// corrupting Data here would prove nothing.
	corrupted := false
	for i, op := range proj.Ops {
		if op.Kind != opArtifact || op.Artifact == nil {
			continue
		}
		mutated := *op.Artifact
		mutated.Name = "tampered/" + mutated.Name
		proj.Ops[i].Artifact = &mutated
		corrupted = true
		break
	}
	if !corrupted {
		t.Fatal("history carries no artifact op, so this test cannot corrupt one — the fixture no longer covers the risk")
	}

	divergence, err := DiffLiveJournal(live, proj)
	if err != nil {
		t.Fatalf("DiffLiveJournal: %v", err)
	}
	if divergence == "" {
		t.Fatal("a corrupted artifact verified clean: excluding artifact.recorded retired the artifact check instead of de-noising it (#4244)")
	}
}

func containsType(events []journal.Event, typ journal.EventType) bool {
	return countType(events, typ) > 0
}

func countType(events []journal.Event, typ journal.EventType) (n int) {
	for _, ev := range events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// projectedEvents re-projects proj the way DiffLiveJournal does, so a test
// can assert on the side of the comparison the live journal is measured
// against.
func reprojectedEvents(t *testing.T, proj JournalProjection) []journal.Event {
	t.Helper()
	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// --- rate ceiling (#4244's saturation half) ---

type recordedDivergence struct {
	runID  string
	detail string
}

func collectingReporter(into *[]recordedDivergence) DivergenceReporter {
	return func(runID, detail string) {
		*into = append(*into, recordedDivergence{runID, detail})
	}
}

func shapeDivergence(index int, live, projected string) string {
	return fmt.Sprintf("normative event %d diverges:\n  live:      type=%s name=cli-on-pod/stdout.log\n  projected: type=%s",
		index, live, projected)
}

// TestRateLimiterFilesVerbatimUnderTheCeiling: a guard that summarizes its
// first finding is useless. Everything under the ceiling passes through
// unchanged.
func TestRateLimiterFilesVerbatimUnderTheCeiling(t *testing.T) {
	var got []recordedDivergence
	clock := time.Unix(0, 0).UTC()
	limiter := NewDivergenceRateLimiter(collectingReporter(&got), 3, time.Hour, func() time.Time { return clock })

	for i := 1; i <= 3; i++ {
		limiter.Report(fmt.Sprintf("run-%d", i), shapeDivergence(i, "artifact.recorded", "stage.finished"))
	}
	limiter.Flush()

	if len(got) != 3 {
		t.Fatalf("filed %d divergences under a ceiling of 3, want 3: %+v", len(got), got)
	}
	for i, rec := range got {
		if want := fmt.Sprintf("run-%d", i+1); rec.runID != want {
			t.Errorf("filing %d names run %q, want %q", i, rec.runID, want)
		}
		if !strings.Contains(rec.detail, "normative event") {
			t.Errorf("filing %d was summarized rather than passed through: %q", i, rec.detail)
		}
	}
}

// TestRateLimiterAggregatesAboveTheCeiling is the acceptance criterion: past
// the ceiling the storm itself becomes the alarm, naming its count and top
// signatures, instead of ten thousand indistinguishable filings.
func TestRateLimiterAggregatesAboveTheCeiling(t *testing.T) {
	var got []recordedDivergence
	clock := time.Unix(0, 0).UTC()
	limiter := NewDivergenceRateLimiter(collectingReporter(&got), 2, time.Hour, func() time.Time { return clock })

	// Two pass through, then a boot-sweep storm of the two measured shapes.
	limiter.Report("run-a", shapeDivergence(4, "artifact.recorded", "stage.finished"))
	limiter.Report("run-b", shapeDivergence(4, "artifact.recorded", "stage.finished"))
	for i := 0; i < 900; i++ {
		limiter.Report("run-storm", shapeDivergence(4+i, "artifact.recorded", "stage.finished"))
	}
	for i := 0; i < 100; i++ {
		limiter.Report("run-storm", shapeDivergence(7+i, "agent.lifecycle", "stage.finished"))
	}
	limiter.Flush()

	if len(got) != 4 {
		t.Fatalf("1002 divergences produced %d filings, want 4 (2 verbatim + 1 ceiling + 1 aggregate): %+v", len(got), got)
	}
	ceiling := got[2].detail
	if !strings.Contains(ceiling, "rate ceiling reached") {
		t.Errorf("third filing is not the ceiling alarm: %q", ceiling)
	}
	aggregate := got[3].detail
	if !strings.Contains(aggregate, "aggregated 1000 suppressed divergences") {
		t.Errorf("aggregate does not name the suppressed count: %q", aggregate)
	}
	if !strings.Contains(aggregate, "live=artifact.recorded projected=stage.finished ×900") {
		t.Errorf("aggregate does not name the dominant signature and its count: %q", aggregate)
	}
	if !strings.Contains(aggregate, "live=agent.lifecycle projected=stage.finished ×100") {
		t.Errorf("aggregate does not name the secondary signature and its count: %q", aggregate)
	}
}

// TestRateLimiterFlushKeepsTheCeilingClosed: Flush summarizes what has piled
// up, it does not hand back the window's budget. Otherwise the reconcile
// loop's per-pass flush would reopen the floodgate on every tick and the
// ceiling would never bind.
func TestRateLimiterFlushKeepsTheCeilingClosed(t *testing.T) {
	var got []recordedDivergence
	clock := time.Unix(0, 0).UTC()
	limiter := NewDivergenceRateLimiter(collectingReporter(&got), 1, time.Hour, func() time.Time { return clock })

	limiter.Report("run-a", shapeDivergence(1, "artifact.recorded", "stage.finished"))
	limiter.Report("run-b", shapeDivergence(2, "artifact.recorded", "stage.finished"))
	limiter.Flush()
	before := len(got)

	clock = clock.Add(30 * time.Minute) // still inside the window
	limiter.Report("run-c", shapeDivergence(3, "artifact.recorded", "stage.finished"))
	limiter.Flush()

	for _, rec := range got[before:] {
		if strings.Contains(rec.detail, "normative event 3") {
			t.Fatalf("Flush reopened the ceiling mid-window: %q", rec.detail)
		}
	}

	// A new window does hand the budget back — a storm an hour ago must not
	// silence a genuine divergence today.
	clock = clock.Add(time.Hour)
	limiter.Report("run-d", shapeDivergence(9, "artifact.recorded", "stage.finished"))
	last := got[len(got)-1]
	if last.runID != "run-d" || !strings.Contains(last.detail, "normative event 9") {
		t.Fatalf("a new window did not restore verbatim filing: %+v", last)
	}
}

// TestDivergenceSignatureCollapsesPerRunParticulars: the aggregate is only
// useful if ten thousand instances of one shape collapse to one line. The
// normative index and artifact name vary per run and must not enter the
// signature; a shape the differ can emit but this function does not model
// must still degrade to something bounded rather than to the raw detail.
func TestDivergenceSignatureCollapsesPerRunParticulars(t *testing.T) {
	for name, tc := range map[string]struct{ detail, want string }{
		"index and name vary": {
			shapeDivergence(41, "artifact.recorded", "ref.touched"),
			"live=artifact.recorded projected=ref.touched",
		},
		"length mismatch": {
			"normative view lengths diverge (live 12, projected 11); first extra live event: type=artifact.recorded name=x",
			"length-mismatch extra=artifact.recorded",
		},
		"unmodelled shape falls back to its leading line": {
			"history projects identity web/abc, journal directory is web/def",
			"history projects identity web/abc, journal directory is web/def",
		},
	} {
		if got := divergenceSignature(tc.detail); got != tc.want {
			t.Errorf("%s: divergenceSignature = %q, want %q", name, got, tc.want)
		}
	}
	if got := divergenceSignature(shapeDivergence(7, "artifact.recorded", "ref.touched")); got != divergenceSignature(shapeDivergence(9001, "artifact.recorded", "ref.touched")) {
		t.Error("signature varies with the normative index, so one shape cannot aggregate")
	}
	long := divergenceSignature(strings.Repeat("x", 400))
	if len(long) > maxSignatureLine+len("…") {
		t.Errorf("fallback signature is unbounded (%d chars)", len(long))
	}
}

// TestNilRateLimiterIsInert covers the daemon's own shape: no live-journal
// registry means no reporter, and the loop calls Flush unconditionally.
func TestNilRateLimiterIsInert(t *testing.T) {
	if limiter := NewDivergenceRateLimiter(nil, 1, time.Hour, nil); limiter != nil {
		t.Fatalf("wrapping a nil reporter produced %v, want nil", limiter)
	}
	var limiter *DivergenceRateLimiter
	limiter.Report("run", "detail")
	limiter.Flush()
}

func spliceAfter(t *testing.T, events []journal.Event, after journal.EventType, insert journal.Event) []journal.Event {
	t.Helper()
	for i, ev := range events {
		if ev.Type != after {
			continue
		}
		out := make([]journal.Event, 0, len(events)+1)
		out = append(out, events[:i+1]...)
		out = append(out, insert)
		return append(out, events[i+1:]...)
	}
	t.Fatalf("no %s event to splice after", after)
	return nil
}
