package main

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

func TestLabelAbortedRunPRFailedEventDoesNotSuppressRetry(t *testing.T) {
	runsDir, runID, jr := newRunAbortLabelJournal(t, "42")
	calls := 0
	label := func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
		calls++
		if calls == 1 {
			return providers.WorkItem{}, &providers.RateLimitError{Status: 403, RemainingKnown: true, Reset: time.Now().Add(time.Minute)}
		}
		return providers.WorkItem{}, nil
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	if err := labelAbortedRunPR(runsDir, runID, journal.PhaseAborted, jr, repo, label); err == nil {
		t.Fatal("expected quota")
	}
	if err := labelAbortedRunPR(runsDir, runID, journal.PhaseAborted, jr, repo, label); err != nil {
		t.Fatal(err)
	}
	if err := labelAbortedRunPR(runsDir, runID, journal.PhaseAborted, jr, repo, label); err != nil {
		t.Fatal(err)
	}
	events := runAbortLabelEvents(t, runsDir, runID)
	if calls != 2 || len(events) != 2 || events[0].Error == nil || events[1].Error != nil {
		t.Fatalf("calls=%d events=%+v", calls, events)
	}
}

func TestLabelAbortedRunPRDurablyDefersQuota(t *testing.T) {
	runsDir, runID, jr := newRunAbortLabelJournal(t, "42")
	now := time.Now()
	q := &terminalreport.Queue{Dir: t.TempDir(), Gaggle: "test", Now: func() time.Time { return now }}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	if err := labelAbortedRunPR(runsDir, runID, journal.PhaseAborted, jr, repo, func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
		return providers.WorkItem{}, &providers.RateLimitError{Reset: now.Add(time.Minute)}
	}, q); err == nil {
		t.Fatal("expected deferral")
	}
	now = now.Add(2 * time.Minute)
	calls := 0
	if err := q.Reconcile(context.Background(), 8, func(_ context.Context, e terminalreport.Effect) error {
		calls++
		if e.Kind != terminalreport.AbortLabel || e.ItemID != "42" || e.RunID != runID || e.Repository != repo || e.Gaggle != "test" {
			t.Fatalf("wrong target: %+v", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("replayed=%d", calls)
	}
}

func TestScrubTerminalErrorRetainsQuotaGuidance(t *testing.T) {
	reg := journal.NewRegistryScrubber()
	reg.Register([]byte("test-secret"))
	reset := time.Now().Add(time.Hour)
	err := scrubTerminalError(reg, &providers.RateLimitError{Endpoint: "https://example.test/test-secret", Reset: reset, RetryAfterRaw: "120", Secondary: true})
	limit, ok := providers.AsRateLimitError(err)
	if !ok || !limit.Reset.Equal(reset) || limit.RetryAfterRaw != "120" || !limit.Secondary {
		t.Fatalf("lost retry metadata: %v", err)
	}
	if limit.Endpoint == "https://example.test/test-secret" {
		t.Fatal("leaked secret")
	}
}
