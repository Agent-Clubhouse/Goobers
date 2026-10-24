package escalationnotify

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

func createRunJournal(t *testing.T, runsDir, runID string) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: "acme-web"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func TestAttributionContextForRunReadsJournalIdentity(t *testing.T) {
	runsDir := t.TempDir()
	createRunJournal(t, runsDir, "run-1")
	ctx, err := AttributionContextForRun(context.Background(), runsDir, "run-1", "ci-poll")
	if err != nil {
		t.Fatalf("AttributionContextForRun: %v", err)
	}
	got, ok := providers.AttributionFromContext(ctx)
	want := providers.Attribution{
		Schema: 1, Goobers: true, Gaggle: "acme-web", Workflow: "implementation",
		Task: "ci-poll", Goober: "runner", Run: "run-1",
	}
	if !ok || got != want {
		t.Fatalf("attribution = %+v (ok=%v), want %+v", got, ok, want)
	}

	base := context.Background()
	if ctx, err := AttributionContextForRun(base, runsDir, "missing", "ci-poll"); err == nil || ctx != base {
		t.Fatalf("missing run: ctx changed=%v err=%v, want the input ctx and an error", ctx != base, err)
	}
}

// TestTerminalHandlersAttributeProviderWrites is #5178: a terminal handler
// attributes its writes to the run — retasking an attribution the runner
// already attached, else loading the run's journal identity — and stays
// unattributed (fail-closed) when neither exists.
func TestTerminalHandlersAttributeProviderWrites(t *testing.T) {
	p, poster, _ := newTestPolicy(t)
	createRunJournal(t, p.RunsDir, "run-1")

	attributed := providers.WithAttributionContext(context.Background(), providers.Attribution{Schema: 1, Goobers: true, Run: "run-1", Task: "implement", Goober: "implementer"})
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		runID    string
		wantOK   bool
		wantTask string
	}{
		{name: "runner attached", ctx: attributed, runID: "run-1", wantOK: true, wantTask: "park"},
		{name: "from journal", ctx: context.Background(), runID: "run-1", wantOK: true, wantTask: "park"},
		{name: "no journal", ctx: context.Background(), runID: "run-2"},
		{name: "no run id", ctx: context.Background()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			poster.updates, poster.ctxs = nil, nil
			if err := p.Blocked(tc.ctx, runner.BlockedOutcome{RunID: tc.runID, RepoRef: webRepoRef, Stage: "park", ItemID: "42"}); err != nil {
				t.Fatalf("Blocked: %v", err)
			}
			got, ok := providers.AttributionFromContext(poster.ctxs[0])
			if ok != tc.wantOK || (ok && (got.Task != tc.wantTask || got.Goober != "runner")) {
				t.Fatalf("attribution = %+v (ok=%v), want ok=%v task=%q goober=runner", got, ok, tc.wantOK, tc.wantTask)
			}
		})
	}
}

// TestTerminalAttributionMatchesTheJournalLookupOnTheLocalPath pins that routing
// the local TerminalNotifier through terminalHandlerAttributionContext (so the
// cloud can supply the run's attribution up front) changed nothing for the
// daemon: with no attribution on the incoming context — the only context the
// notifier gives it — both paths yield the identical attribution, and both leave
// the context untouched for a run with no readable journal or no run id.
func TestTerminalAttributionMatchesTheJournalLookupOnTheLocalPath(t *testing.T) {
	p, _, _ := newTestPolicy(t)
	createRunJournal(t, p.RunsDir, "run-1")
	base := context.Background()
	for _, runID := range []string{"run-1", "missing", ""} {
		old, _ := AttributionContextForRun(base, p.RunsDir, runID, "implement")
		got := p.terminalHandlerAttributionContext(base, runID, "implement")
		oldAttr, oldOK := providers.AttributionFromContext(old)
		gotAttr, gotOK := providers.AttributionFromContext(got)
		if oldOK != gotOK || oldAttr != gotAttr {
			t.Errorf("run %q: journal lookup = %+v (%v), terminal handler = %+v (%v)", runID, oldAttr, oldOK, gotAttr, gotOK)
		}
		if !oldOK && (old != base || got != base) {
			t.Errorf("run %q: an unattributable run changed the context", runID)
		}
	}
}
