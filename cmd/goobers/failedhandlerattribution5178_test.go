package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// attributionRecordingCommenter is a provider fake that, like the real
// providers, accepts run attribution — so escalationCommenter's daemon-write
// guard is exercised instead of skipped.
type attributionRecordingCommenter struct {
	blockedHandlerFakeCommenter
	attributions []providers.Attribution
}

func (f *attributionRecordingCommenter) SetAttribution(a providers.Attribution) {
	f.attributions = append(f.attributions, a)
}

func setupFailedHandlerAttributionTest(t *testing.T) (instance.Layout, *instance.Config, *attributionRecordingCommenter) {
	t.Helper()
	fake := &attributionRecordingCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	return l, cfg, fake
}

// seedFailedRunForAttributionTest claims the item for runID and, when
// withJournal, gives the run a durable identity. It returns the release.
func seedFailedRunForAttributionTest(t *testing.T, l instance.Layout, runID string, withJournal bool) func() {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("5178", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim %s: ok=%v err=%v", runID, ok, err)
	}
	seedItemRepositoryForTest(t, l, runID, "5178", providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"})
	if withJournal {
		jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: "acme-web"}, nil)
		if err != nil {
			t.Fatalf("journal.Create %s: %v", runID, err)
		}
		if err := jr.Close(); err != nil {
			t.Fatalf("close journal %s: %v", runID, err)
		}
	}
	return func() {
		if err := ledger.Release("5178", runID); err != nil {
			t.Fatalf("release claim %s: %v", runID, err)
		}
	}
}

func failedOutcomeForAttributionTest(runID string) runner.FailedOutcome {
	return runner.FailedOutcome{
		RunID:   runID,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage:   "ci-poll",
		Code:    telemetry.ErrCodeTimeout,
	}
}

// TestFailedHandlerAttributesWritesFromRunJournal is #5178's regression: a
// failed run whose terminal handling arrives without context attribution (a
// post-PR stage failure on a path that never attached it) must still post its
// failure-streak comment and, at the threshold, apply the circuit breaker —
// attributed to the originating run from its durable journal identity, not
// refused by the daemon-write attribution guard.
func TestFailedHandlerAttributesWritesFromRunJournal(t *testing.T) {
	l, cfg, fake := setupFailedHandlerAttributionTest(t)
	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	for _, runID := range []string{"run-attr-1", "run-attr-2", "run-attr-3"} {
		release := seedFailedRunForAttributionTest(t, l, runID, true)
		before := len(fake.attributions)
		if err := h(context.Background(), failedOutcomeForAttributionTest(runID)); err != nil {
			t.Fatalf("failed handler for %s: %v", runID, err)
		}
		release()
		if len(fake.attributions) == before {
			t.Fatalf("run %s: no attributed provider write", runID)
		}
		for _, a := range fake.attributions[before:] {
			if a.Run != runID || a.Gaggle != "acme-web" || a.Workflow != "implementation" || a.Task != "ci-poll" || a.Goober != "runner" {
				t.Fatalf("run %s: attribution = %+v, want the originating run's identity", runID, a)
			}
		}
	}

	breakerApplied := false
	for _, call := range fake.calls {
		for _, label := range call.AddLabels {
			if label == providers.LabelNeedsHuman {
				breakerApplied = true
			}
		}
	}
	if !breakerApplied {
		t.Fatalf("circuit breaker never applied %s; calls = %+v", providers.LabelNeedsHuman, fake.calls)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("failure-streak comments = %+v, want exactly one comment updated in place", fake.comments)
	}
}

// TestFailedHandlerStaysFailClosedWithoutDurableRun keeps the guard honest: a
// failed outcome with no context attribution AND no readable run journal has
// no durable run to attribute to, so its provider write is still refused.
func TestFailedHandlerStaysFailClosedWithoutDurableRun(t *testing.T) {
	l, cfg, fake := setupFailedHandlerAttributionTest(t)
	seedFailedRunForAttributionTest(t, l, "run-orphan", false)
	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), failedOutcomeForAttributionTest("run-orphan"))
	if err == nil || !strings.Contains(err.Error(), "without run attribution") {
		t.Fatalf("handler error = %v, want the daemon-write attribution refusal", err)
	}
	if len(fake.attributions) != 0 || len(fake.calls) != 0 {
		t.Fatalf("unattributed run reached the provider: attributions=%+v calls=%+v", fake.attributions, fake.calls)
	}
}
