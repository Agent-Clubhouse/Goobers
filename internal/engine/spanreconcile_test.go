package engine

// spanreconcile_test.go is the DS5 two-sided guard for #3805.
//
// span.recorded is deliberately excluded from journal.ConformanceView; the
// EventError that replaces an unadoptable span is NOT, and its code is
// projected. So the live writer and the reconciler's re-projection must have
// the SAME span source or they disagree by exactly one normative event on
// every run carrying a transcript — and DS5 files a live_journal_divergence
// for each of them, a fleet-wide false alarm about blob-store reachability
// dressed up as a run disagreeing with its own history.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// liveWriterWithSpans builds a real live writer for proj's gaggle with src as
// its span source — the daemon shape newLiveJournalWriter produces once the
// blob store is threaded into it.
func liveWriterWithSpans(t *testing.T, gaggle string, src SpanSource) (*livejournal.Writer, string) {
	t.Helper()
	runsDir := filepath.Join(t.TempDir(), "runs")
	opts := []livejournal.Option{}
	if src != nil {
		opts = append(opts, livejournal.WithSpanSource(src))
	}
	w, err := livejournal.NewWriter(func(g string) (string, bool) {
		if g != gaggle {
			return "", false
		}
		return runsDir, true
	}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w, runsDir
}

// assertJournalAdoptedSpan reads a journal on disk and requires that whoever
// wrote it — the live writer, or the reconciler's repair projection — really
// did resolve the span op to a span.recorded whose bytes read back identical,
// recording no span_unavailable. For the live cases below this is the FIRST
// half, without which the second would pass vacuously: two journals with no
// span in either also agree.
func assertJournalAdoptedSpan(t *testing.T, runsDir, runID string, want []byte) {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var span *journal.Event
	for i := range events {
		if events[i].Type == journal.EventSpanRecorded {
			span = &events[i]
		}
		if events[i].Type == journal.EventError && events[i].Error != nil &&
			events[i].Error.Code == spanUnavailableErrorCode {
			t.Fatalf("journal recorded %s despite a configured span source: %+v",
				spanUnavailableErrorCode, events[i])
		}
	}
	if span == nil || span.Ref == nil {
		t.Fatalf("journal has no span.recorded event; events = %+v", events)
	}
	got, err := rd.SpanBytes(*span.Ref)
	if err != nil {
		t.Fatalf("SpanBytes: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("adopted span bytes = %q, want %q", got, want)
	}
}

// TestReconcilerWithSpanSourceVerifiesAnAdoptedSpanWithoutDivergence is
// #3805's regression test for the divergence storm: a run whose ONLY
// difference between the two sides would be the span must reconcile clean
// when both sides carry the same source.
//
// It fails if the source reaches only one of the reconciler's two projection
// call sites — the verification re-projection is the one that decides this
// test, and it is the one completed_runs.go passed no options to.
func TestReconcilerWithSpanSourceVerifiesAnAdoptedSpanWithoutDivergence(t *testing.T) {
	data := []byte(`{"event":"prompt","adapter":"copilot-cli"}`)
	proj := singleTranscriptStageProj(t, "live-span-verify", data)
	src := &fakeSpanSource{blobs: map[string][]byte{journal.Digest(data): data}}

	writer, runsDir := liveWriterWithSpans(t, proj.Identity.Gaggle, src)
	liveAuthor(t, writer, proj, len(proj.Ops))
	assertJournalAdoptedSpan(t, runsDir, proj.Identity.RunID, data)

	var divergences []string
	reconciler, fake, _ := reconcilerFor(t, proj, runsDir, writer, &divergences)
	reconciler = reconciler.WithSpanSource(src)

	count, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if count != 0 {
		t.Fatalf("Reconcile projected %d runs; a complete live-authored journal is verified, not rewritten", count)
	}
	if fake.queries != 1 {
		t.Fatalf("projection queries = %d, want exactly the verification query", fake.queries)
	}
	if len(divergences) != 0 {
		t.Fatalf("a run whose only cross-side difference is an adopted span filed divergences:\n%v", divergences)
	}
}

// TestReconcilerWithSpanSourceAdoptsSpansInTheRepairProjection is the OTHER
// half WithSpanSource's contract names: the repair/backfill WRITE.
//
// The verification re-projection above decides whether DS5 files a false
// divergence; this one decides what a recovered run's journal actually
// CONTAINS. A run with no live-authored journal — the crash-orphan the daemon
// never wrote, and every engine run projected before the live writer existed —
// is reconstructed from history by projectCompletedRun, and if the option list
// does not reach THAT call the recovered journal permanently carries
// span_unavailable in place of the transcript the blob store is still holding.
//
// Without this test the ablation survives: dropping r.projectOpts from
// reconcileRun's projectCompletedRun call left the whole engine suite green,
// because every other span-reconcile case is live-authored and takes the
// verification branch instead.
func TestReconcilerWithSpanSourceAdoptsSpansInTheRepairProjection(t *testing.T) {
	data := []byte(`{"event":"prompt","adapter":"copilot-cli"}`)
	proj := singleTranscriptStageProj(t, "repair-span-project", data)
	src := &fakeSpanSource{blobs: map[string][]byte{journal.Digest(data): data}}

	// No live journal on disk and no live registry: the crash-orphan / legacy
	// shape, which is the branch that writes rather than verifies.
	runsDir := filepath.Join(t.TempDir(), "runs")
	var divergences []string
	reconciler, _, _ := reconcilerFor(t, proj, runsDir, nil, &divergences)
	reconciler = reconciler.WithSpanSource(src)

	count, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if count != 1 {
		t.Fatalf("Reconcile projected %d runs, want 1: a run with no journal on disk is repaired, not verified", count)
	}
	// The same read the live-adoption assertion makes, against the journal the
	// REPAIR path wrote.
	assertJournalAdoptedSpan(t, runsDir, proj.Identity.RunID, data)
	if len(divergences) != 0 {
		t.Fatalf("a first projection filed divergences: %v", divergences)
	}
}

// TestReconcilerWithoutSpanSourceFilesAdoptedSpanAsDivergence pins the hazard
// the test above guards, so the two-sidedness is a stated property rather
// than an accident of wiring order: give the live writer a source and the
// reconciler none, and the verification re-projection substitutes a
// conformance-NORMATIVE span_unavailable error event for a span the live
// journal recorded as a NON-normative span.recorded. The views then differ by
// one event and DS5 files it — on every pod-executed agentic run.
func TestReconcilerWithoutSpanSourceFilesAdoptedSpanAsDivergence(t *testing.T) {
	data := []byte(`{"event":"prompt","adapter":"copilot-cli"}`)
	proj := singleTranscriptStageProj(t, "live-span-halfwired", data)
	src := &fakeSpanSource{blobs: map[string][]byte{journal.Digest(data): data}}

	writer, runsDir := liveWriterWithSpans(t, proj.Identity.Gaggle, src)
	liveAuthor(t, writer, proj, len(proj.Ops))
	assertJournalAdoptedSpan(t, runsDir, proj.Identity.RunID, data)

	var divergences []string
	// Deliberately NOT WithSpanSource: the half-wired daemon.
	reconciler, _, _ := reconcilerFor(t, proj, runsDir, writer, &divergences)
	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(divergences) != 1 {
		t.Fatalf("half-wired reconciler filed %d divergences, want exactly 1: %v", len(divergences), divergences)
	}
}
