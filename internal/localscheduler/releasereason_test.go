package localscheduler

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/journal"
)

func openReasonLedger(t *testing.T, now *time.Time) (*ClaimLedger, string, string) {
	t.Helper()
	dir := t.TempDir()
	schedulerDir := filepath.Join(dir, "scheduler")
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	path := filepath.Join(dir, "claims.json")
	ledger, err := OpenClaimLedger(path, WithInstanceLog(log), WithLedgerClock(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	return ledger, path, schedulerDir
}

func releaseEvents(t *testing.T, schedulerDir string) []journal.Event {
	t.Helper()
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	var released []journal.Event
	for _, event := range events {
		if event.Type == journal.EventClaimReleased || event.Type == journal.EventClaimForceReleased {
			released = append(released, event)
		}
	}
	return released
}

func TestReleaseRecordsReasonOnHistoryAndJournalEvent(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ledger, path, schedulerDir := openReasonLedger(t, &now)
	key := ClaimKey{Gaggle: "alpha", Provider: "github", ExternalID: "42"}
	if ok, _, err := ledger.ClaimScoped(key, "run-a", "implement", time.Hour); err != nil || !ok {
		t.Fatalf("ClaimScoped: ok=%v err=%v", ok, err)
	}
	reason := claimrelease.Failed.Because("DEPENDENCY_NOT_MET", "registry unreachable from /home/op/cache/x").At("implement")
	if err := ledger.ReleaseScoped(key, "run-a", reason); err != nil {
		t.Fatal(err)
	}

	want := reason.Normalize()
	if strings.Contains(want.Summary, "/home/op") {
		t.Fatalf("normalized summary kept a local path: %q", want.Summary)
	}
	history := ledger.HistoryForRun("run-a")
	if len(history) != 1 || history[0].ReleaseReason != want {
		t.Fatalf("history = %+v, want one entry with reason %+v", history, want)
	}
	reopened, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.HistoryForRun("run-a"); len(got) != 1 || got[0].ReleaseReason != want {
		t.Fatalf("persisted history = %+v", got)
	}
	events := releaseEvents(t, schedulerDir)
	if len(events) != 1 {
		t.Fatalf("release events = %+v", events)
	}
	if events[0].Reason != string(claimrelease.Failed) || events[0].ClaimRelease == nil || *events[0].ClaimRelease != want {
		t.Fatalf("claim.released carries reason %q / %+v, want %+v", events[0].Reason, events[0].ClaimRelease, want)
	}
}

func TestRetriedReleaseNeverRecordsAContradictoryReason(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ledger, _, schedulerDir := openReasonLedger(t, &now)
	if ok, _, err := ledger.Claim("7", "run-a", "implement", time.Hour); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	first := claimrelease.Completed.Because("closed_out", "")
	if err := ledger.Release("7", "run-a", first); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := ledger.Release("7", "run-a", claimrelease.Failed.Because("late_retry", "")); err != nil {
		t.Fatal(err)
	}
	if history := ledger.HistoryForRun("run-a"); len(history) != 1 || history[0].ReleaseReason != first.Normalize() {
		t.Fatalf("retried release rewrote history: %+v", history)
	}
	if events := releaseEvents(t, schedulerDir); len(events) != 1 {
		t.Fatalf("retried release journaled a second boundary: %+v", events)
	}
}

func TestClaimReleaseClaimSequenceKeepsEachReason(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ledger, _, schedulerDir := openReasonLedger(t, &now)
	key := ClaimKey{Gaggle: "alpha", Provider: "github", ExternalID: "9"}
	steps := []struct {
		run    string
		reason claimrelease.Reason
	}{
		{"run-a", claimrelease.Failed.Because("run_failed", "tests failed").At("verify")},
		{"run-b", claimrelease.NoWork.Reason()},
		{"run-c", claimrelease.Completed.Because("closed_out", "")},
	}
	for _, step := range steps {
		if ok, _, err := ledger.ClaimScoped(key, step.run, "implement", time.Hour); err != nil || !ok {
			t.Fatalf("ClaimScoped(%s): ok=%v err=%v", step.run, ok, err)
		}
		now = now.Add(time.Minute)
		if err := ledger.ReleaseScoped(key, step.run, step.reason); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	history := ledger.HistoryForItem("9")
	if len(history) != len(steps) {
		t.Fatalf("history = %+v", history)
	}
	for i, entry := range history { // newest first
		step := steps[len(steps)-1-i]
		if entry.RunID != step.run || entry.ReleaseReason != step.reason.Normalize() {
			t.Fatalf("history[%d] = %s %+v, want %s %+v", i, entry.RunID, entry.ReleaseReason, step.run, step.reason)
		}
	}
	events := releaseEvents(t, schedulerDir)
	if len(events) != len(steps) {
		t.Fatalf("release events = %+v", events)
	}
	for i, event := range events {
		if event.RunID != steps[i].run || event.ClaimRelease == nil || event.ClaimRelease.Disposition != steps[i].reason.Disposition {
			t.Fatalf("event %d = %+v", i, event)
		}
	}
}

func TestRecoverExpiredRecordsStallSwept(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ledger, _, schedulerDir := openReasonLedger(t, &now)
	if ok, _, err := ledger.Claim("5", "run-a", "implement", time.Minute); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	now = now.Add(time.Hour)
	released, err := ledger.RecoverExpired(now)
	if err != nil || len(released) != 1 {
		t.Fatalf("RecoverExpired = %+v, %v", released, err)
	}
	if history := ledger.HistoryForRun("run-a"); history[0].ReleaseReason != StallSweptReason() {
		t.Fatalf("history reason = %+v", history[0].ReleaseReason)
	}
	events := releaseEvents(t, schedulerDir)
	if len(events) != 1 || events[0].ClaimRelease == nil || events[0].ClaimRelease.Disposition != claimrelease.StallSwept ||
		events[0].ClaimRelease.Code != ReleaseCodeLeaseExpired {
		t.Fatalf("events = %+v", events)
	}
}

func TestForceReleaseRecordsActor(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ledger, _, schedulerDir := openReasonLedger(t, &now)
	if ok, _, err := ledger.Claim("6", "run-a", "implement", time.Hour); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.ForceRelease("6"); err != nil {
		t.Fatal(err)
	}
	want := claimrelease.Reason{Disposition: claimrelease.ForceReleased, Actor: "cli"}
	if history := ledger.HistoryForRun("run-a"); history[0].ReleaseReason != want {
		t.Fatalf("history reason = %+v", history[0].ReleaseReason)
	}
	events := releaseEvents(t, schedulerDir)
	if len(events) != 1 || events[0].Type != journal.EventClaimForceReleased || events[0].Runner["actor"] != "cli" ||
		events[0].ClaimRelease == nil || *events[0].ClaimRelease != want {
		t.Fatalf("events = %+v", events)
	}
}

func TestLedgerWithoutReleaseReasonsStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claims.json")
	released := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	legacy := `{"schema":"goobers.dev/scheduler/claims/v1","entries":{},"history":{"run-old":{"7":{` +
		`"verification":{"state":"unverified","observedAt":"0001-01-01T00:00:00Z"},"itemId":"7","runId":"run-old",` +
		`"workflow":"implement","claimedAt":"` + released + `","expiresAt":"` + released + `","releasedAt":"` + released + `"}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatalf("legacy ledger without release reasons failed to load: %v", err)
	}
	history := ledger.HistoryForItem("7")
	if len(history) != 1 || !history[0].ReleaseReason.IsZero() || history[0].ReleaseReason.Normalize().Disposition != claimrelease.Unknown {
		t.Fatalf("legacy history = %+v", history)
	}
}

func finishedRun(status, disposition string, cause *journal.TerminalCause) []journal.Event {
	return []journal.Event{
		{Schema: journal.EventSchema, Seq: 1, Type: journal.EventRunStarted},
		{Schema: journal.EventSchema, Seq: 2, Type: journal.EventRunFinished, Status: status, Disposition: disposition, TerminalCause: cause},
	}
}

func cause(phase journal.RunPhase, classification journal.TerminalClassification, stage, code, message string) *journal.TerminalCause {
	c := &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: phase, Classification: classification,
		SelectorKind: "condition", Code: code, Message: message}
	if stage != "" {
		c.SelectorKind, c.Selector = "stage", stage
	}
	return c
}

func TestReleaseReasonFromRunEvents(t *testing.T) {
	abort := journal.OperatorAbortEvent(1)
	abort.Schema, abort.Seq = journal.EventSchema, 1
	resumed := append(finishedRun("failed", journal.RunDispositionProduced, nil),
		journal.Event{Schema: journal.EventSchema, Seq: 3, Type: journal.EventRunResumed})
	tests := []struct {
		name   string
		events []journal.Event
		want   claimrelease.Reason
	}{
		{"completed", finishedRun("completed", journal.RunDispositionProduced, nil), claimrelease.Completed.Reason()},
		{"no-work", finishedRun("completed", journal.RunDispositionNoWork, nil), claimrelease.NoWork.Reason()},
		{"failed with cause", finishedRun("failed", journal.RunDispositionProduced,
			cause(journal.PhaseFailed, journal.TerminalStageFailure, "implement", "agent_exit", "agent exited 2")),
			claimrelease.Failed.Because("agent_exit", "agent exited 2").At("implement")},
		{"escalated", finishedRun("escalated", journal.RunDispositionProduced,
			cause(journal.PhaseEscalated, journal.TerminalEscalation, "implement", "DEPENDENCY_NOT_MET", "registry access is unavailable")),
			claimrelease.Escalated.Because("DEPENDENCY_NOT_MET", "registry access is unavailable").At("implement")},
		{"operator abort", []journal.Event{abort},
			claimrelease.Cancelled.Because("run_canceled", "run aborted by operator request")},
		{"defined abort", finishedRun("aborted", journal.RunDispositionProduced,
			cause(journal.PhaseAborted, journal.TerminalDefinedAbort, "", "abort_branch", "")),
			claimrelease.Abandoned.Because("abort_branch", "")},
		{"resumed after finish", resumed, claimrelease.Unknown.Because(ReleaseCodeRunNotTerminal, "")},
		{"never finished", nil, claimrelease.Unknown.Because(ReleaseCodeRunNotTerminal, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReleaseReasonFromRunEvents(tt.events); got != tt.want.Normalize() {
				t.Fatalf("ReleaseReasonFromRunEvents = %+v, want %+v", got, tt.want.Normalize())
			}
		})
	}
}

func TestTerminalReleaseReasonReportsAnUnreadableJournalAsUnknown(t *testing.T) {
	got := TerminalReleaseReason(func(string) (string, error) { return "", errors.New("gone") }, "run-x")
	if got.Disposition != claimrelease.Unknown || got.Code != ReleaseCodeRunJournalUnavailable {
		t.Fatalf("TerminalReleaseReason = %+v", got)
	}
}
