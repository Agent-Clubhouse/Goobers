package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readprobe"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

func TestListStatusRunsProjectsOperatorSummary(t *testing.T) {
	service, layout, machine := fixtureService(t)
	startedAt := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	run, clock := createFixtureRun(
		t, layout, machine, "operator-run", "implementation", "goobers",
		startedAt, journal.Trigger{Kind: journal.TriggerItem, Ref: "3088"}, true,
	)
	clock.now = startedAt.Add(time.Minute)
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "query-backlog", Status: "success",
		Outputs: map[string]any{
			"id": "3088", "title": "Operator status cannot answer run progress",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "open-pr", Status: "success",
		Outputs: map[string]any{"id": "4001", "title": "PR title must not replace issue title"},
	}); err != nil {
		t.Fatal(err)
	}
	clock.now = startedAt.Add(2 * time.Minute)
	if err := run.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: "3088"},
		Runner:      map[string]any{"operation": "claim"},
	}); err != nil {
		t.Fatal(err)
	}
	clock.now = startedAt.Add(3 * time.Minute)
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implementation"}); err != nil {
		t.Fatal(err)
	}
	clock.now = startedAt.Add(4 * time.Minute)
	if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "implementation"}); err != nil {
		t.Fatal(err)
	}
	verdictData, err := json.Marshal(map[string]any{
		"decision":  "needs-changes",
		"rationale": "Add operator-facing claim drift.",
	})
	if err != nil {
		t.Fatal(err)
	}
	verdictRef, err := run.RecordArtifact("review-verdict.json", verdictData)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = startedAt.Add(5 * time.Minute)
	if err := run.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review", Verdict: "needs-changes", Ref: &verdictRef,
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "local-ci", Verdict: "pass",
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: "provider.rate_limit", Message: "quota exhausted"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implementation"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return startedAt.Add(5 * time.Minute) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ok, _, err := ledger.ClaimScoped(
		localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "3088"},
		"operator-run", "implementation", time.Hour,
	)
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	service.now = func() time.Time { return startedAt.Add(6 * time.Minute) }
	service.sources.WorkItemLookup = func(context.Context, string, string) (providers.WorkItem, error) {
		return providers.WorkItem{
			Title:  "Operator status cannot answer run progress",
			Labels: []string{providers.LabelClaimed},
		}, nil
	}

	runs, err := service.ListStatusRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	got := runs[0].Operator
	if got.Issue == nil || got.Issue.Number != "3088" ||
		got.Issue.Title != "Operator status cannot answer run progress" {
		t.Fatalf("issue = %+v", got.Issue)
	}
	if got.CurrentStage != "implementation" || got.Trajectory != "implementing" ||
		got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(startedAt.Add(4*time.Minute)) ||
		got.HeartbeatAgeMillis == nil || *got.HeartbeatAgeMillis != (2*time.Minute).Milliseconds() {
		t.Fatalf("liveness = %+v", got)
	}
	if got.Claim.LeaseStatus != "active" || got.Claim.ProviderMarker != "verified" {
		t.Fatalf("claim = %+v", got.Claim)
	}
	if got.LatestError == nil || got.LatestError.Code != "provider.rate_limit" ||
		got.Review == nil || got.Review.Verdict != "needs-changes" ||
		got.Review.Rationale != "Add operator-facing claim drift." {
		t.Fatalf("error/review = %+v", got)
	}
	if got.NextTransition != "finish implementation" || len(got.PotentialBlockers) != 2 {
		t.Fatalf("next/blockers = %+v", got)
	}

	service.sources.WorkItemLookup = func(context.Context, string, string) (providers.WorkItem, error) {
		return providers.WorkItem{}, nil
	}
	runs, err = service.ListStatusRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := runs[0].Operator.Claim.ProviderMarker; got != "drift" {
		t.Fatalf("provider marker after label removal = %q, want drift", got)
	}

	service.sources.WorkItemLookup = func(context.Context, string, string) (providers.WorkItem, error) {
		return providers.WorkItem{}, errors.New("provider unavailable")
	}
	runs, err = service.ListStatusRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := runs[0].Operator.Claim.ProviderMarker; got != "unavailable" {
		t.Fatalf("provider marker after lookup failure = %q, want unavailable", got)
	}
}

func TestListStatusRunsProjectsTerminalOperatorSummary(t *testing.T) {
	for _, phase := range []journal.RunPhase{journal.PhaseCompleted, journal.PhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			service, layout, machine := fixtureService(t)
			startedAt := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
			run, clock := createFixtureRun(
				t, layout, machine, "terminal-"+string(phase), "implementation", "goobers",
				startedAt, journal.Trigger{Kind: journal.TriggerItem, Ref: "3088"}, true,
			)
			clock.now = startedAt.Add(time.Minute)
			if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implementation"}); err != nil {
				t.Fatal(err)
			}
			clock.now = startedAt.Add(2 * time.Minute)
			if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "implementation"}); err != nil {
				t.Fatal(err)
			}
			if err := run.Append(journal.Event{
				Type:        journal.EventRefTouched,
				ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: "3088"},
				Runner:      map[string]any{"operation": "claim"},
			}); err != nil {
				t.Fatal(err)
			}
			verdictData, err := json.Marshal(map[string]string{
				"decision": "needs-changes", "rationale": "Terminal review blocker.",
			})
			if err != nil {
				t.Fatal(err)
			}
			verdictRef, err := run.RecordArtifact("review-verdict.json", verdictData)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Append(journal.Event{
				Type: journal.EventGateEvaluated, Gate: "review", Verdict: "needs-changes", Ref: &verdictRef,
			}); err != nil {
				t.Fatal(err)
			}
			if err := run.Append(journal.Event{
				Type:  journal.EventError,
				Error: &journal.ErrorDetail{Code: "review.failed", Message: "changes required"},
			}); err != nil {
				t.Fatal(err)
			}
			finishFixtureRun(t, run, clock, phase)
			service.now = func() time.Time { return startedAt.Add(10 * time.Minute) }

			runs, err := service.ListStatusRuns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 {
				t.Fatalf("runs = %+v", runs)
			}
			got := runs[0].Operator
			if got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(startedAt.Add(2*time.Minute)) ||
				got.HeartbeatAgeMillis == nil || *got.HeartbeatAgeMillis != (8*time.Minute).Milliseconds() ||
				got.Liveness != "terminal" {
				t.Fatalf("liveness = %+v", got)
			}
			if got.Trajectory != "parked" || got.NextTransition != "" ||
				got.Claim.ProviderMarker != "recorded" {
				t.Fatalf("terminal projection = %+v", got)
			}
			if len(got.PotentialBlockers) != 2 ||
				got.PotentialBlockers[0] != "review.failed: changes required" ||
				got.PotentialBlockers[1] != "review needs-changes: Terminal review blocker." {
				t.Fatalf("blockers = %+v", got.PotentialBlockers)
			}
		})
	}
}

func TestOperatorTrajectory(t *testing.T) {
	for stage, want := range map[string]string{
		"query-backlog":            "implementing",
		"gather-implement-context": "implementing",
		"implementation":           "implementing",
		"custom-active-stage":      "implementing",
		"reviewer":                 "review",
		"local-ci":                 "local CI",
		"push-branch":              "push",
		"open-pr":                  "open PR",
		"ci-poll":                  "CI poll",
		"issue-close-out":          "close-out",
	} {
		if got := operatorTrajectory(stage, journal.PhaseRunning); got != want {
			t.Errorf("operatorTrajectory(%q) = %q, want %q", stage, got, want)
		}
	}
	if got := operatorTrajectory("implementation", journal.PhaseCompleted); got != "parked" {
		t.Fatalf("terminal trajectory = %q, want parked", got)
	}
}

func TestDecorateOperatorClaimsVerifiesEveryRunningClaim(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	ledgerNow := now.Add(-2 * time.Hour)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return ledgerNow }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ok, _, err := ledger.ClaimScoped(
		localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "2"},
		"expired", "implementation", time.Hour,
	)
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	ledgerNow = now
	activeKey := localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "1"}
	ok, _, err = ledger.ClaimScoped(activeKey, "active", "implementation", time.Hour)
	if err != nil || !ok {
		t.Fatalf("active claim = %v, %v", ok, err)
	}
	releasedKey := localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "3"}
	ok, _, err = ledger.ClaimScoped(releasedKey, "released", "implementation", time.Hour)
	if err != nil || !ok {
		t.Fatalf("released claim = %v, %v", ok, err)
	}
	if err := ledger.ReleaseScoped(releasedKey, "released"); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	service := &Local{sources: LocalSources{
		Layout: layout,
		WorkItemLookup: func(_ context.Context, _, itemID string) (providers.WorkItem, error) {
			lookups++
			if itemID == "1" {
				return providers.WorkItem{Labels: []string{providers.LabelClaimed}}, nil
			}
			return providers.WorkItem{}, nil
		},
	}}
	runs := []RunSummary{
		{
			ID: "active", Gaggle: "goobers", Phase: journal.PhaseRunning,
			Operator: OperatorRunSummary{
				Issue: &OperatorIssue{Number: "1"},
				Claim: OperatorClaim{LeaseStatus: "none", ProviderMarker: "recorded"},
			},
		},
		{
			ID: "expired", Gaggle: "goobers", Phase: journal.PhaseRunning,
			Operator: OperatorRunSummary{
				Issue: &OperatorIssue{Number: "2"},
				Claim: OperatorClaim{LeaseStatus: "none", ProviderMarker: "recorded"},
			},
		},
		{
			ID: "released", Gaggle: "goobers", Phase: journal.PhaseRunning,
			Operator: OperatorRunSummary{
				Issue: &OperatorIssue{Number: "3"},
				Claim: OperatorClaim{LeaseStatus: "none", ProviderMarker: "recorded"},
			},
		},
		{
			ID: "historical", Gaggle: "goobers", Phase: journal.PhaseCompleted,
			Operator: OperatorRunSummary{
				Issue: &OperatorIssue{Number: "4"},
				Claim: OperatorClaim{LeaseStatus: "none", ProviderMarker: "recorded"},
			},
		},
	}
	if err := service.decorateOperatorClaims(context.Background(), runs, now); err != nil {
		t.Fatal(err)
	}
	if lookups != 3 {
		t.Fatalf("provider lookups = %d, want every running claim", lookups)
	}
	if got := runs[0].Operator.Claim; got.LeaseStatus != "active" || got.ProviderMarker != "verified" {
		t.Fatalf("active claim = %+v", got)
	}
	if got := runs[1].Operator.Claim; got.LeaseStatus != "expired" || got.ProviderMarker != "not-present" {
		t.Fatalf("expired claim = %+v", got)
	}
	if got := runs[2].Operator.Claim; got.LeaseStatus != "released" || got.ProviderMarker != "not-present" {
		t.Fatalf("released claim = %+v", got)
	}
	if got := runs[3].Operator.Claim.ProviderMarker; got != "recorded" {
		t.Fatalf("historical provider marker = %q, want recorded history", got)
	}
}

// TestDecorateOperatorClaimsKeepsReaderCredentialGapOutOfRunBlockers pins the
// #3346 shape exactly: two healthy running runs, claims ACTIVE, markers really
// on the provider — but the status invocation itself has no credential to check
// them. That must surface as a diagnostics limitation, never as the runs' own
// blockers, and it must not disturb the rest of the operator projection.
func TestDecorateOperatorClaimsKeepsReaderCredentialGapOutOfRunBlockers(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 3, 15, 0, 0, time.UTC)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []struct{ id, itemID string }{{"run-a", "3344"}, {"run-b", "3345"}} {
		ok, _, err := ledger.ClaimScoped(
			localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: run.itemID},
			run.id, "implementation", time.Hour,
		)
		if err != nil || !ok {
			t.Fatalf("claim %s = %v, %v", run.id, ok, err)
		}
	}
	credentialGap := errors.New(
		"no credential in GOOBERS_CRED_GITHUB_ISSUES_READ env var — this subcommand " +
			`must run as a stage declaring capabilities: ["github:issues:read"]`,
	)
	service := &Local{sources: LocalSources{
		Layout: layout,
		WorkItemLookup: func(context.Context, string, string) (providers.WorkItem, error) {
			return providers.WorkItem{}, credentialGap
		},
	}}
	runs := []RunSummary{
		{
			ID: "run-a", Gaggle: "goobers", Phase: journal.PhaseRunning,
			Operator: OperatorRunSummary{
				Issue:             &OperatorIssue{Number: "3344"},
				Claim:             OperatorClaim{LeaseStatus: "none", ProviderMarker: "recorded"},
				PotentialBlockers: []string{},
			},
		},
		{
			ID: "run-b", Gaggle: "goobers", Phase: journal.PhaseRunning,
			Operator: OperatorRunSummary{
				Issue:             &OperatorIssue{Number: "3345"},
				Claim:             OperatorClaim{LeaseStatus: "none", ProviderMarker: "recorded"},
				PotentialBlockers: []string{},
			},
		},
	}
	if err := service.decorateOperatorClaims(context.Background(), runs, now); err != nil {
		t.Fatal(err)
	}
	for i := range runs {
		got := runs[i].Operator
		if len(got.PotentialBlockers) != 0 {
			t.Fatalf("run %s blockers = %+v, want none: a reader credential gap is not a run blocker",
				runs[i].ID, got.PotentialBlockers)
		}
		if len(got.DiagnosticsLimitations) != 1 ||
			got.DiagnosticsLimitations[0] != "provider claim marker verification unavailable: "+credentialGap.Error() {
			t.Fatalf("run %s diagnostics = %+v", runs[i].ID, got.DiagnosticsLimitations)
		}
		// The lease is still reported as live and the marker as merely
		// unverified — the run's own state must read healthy.
		if got.Claim.LeaseStatus != "active" || got.Claim.ProviderMarker != "unavailable" {
			t.Fatalf("run %s claim = %+v", runs[i].ID, got.Claim)
		}
	}
}

// TestDecorateOperatorClaimsReportsRealMarkerDriftAsBlocker guards the other
// direction of the #3346 split: genuine claim drift the reader *did* observe
// stays a run blocker and produces no diagnostics limitation.
func TestDecorateOperatorClaimsReportsRealMarkerDriftAsBlocker(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 3, 15, 0, 0, time.UTC)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ok, _, err := ledger.ClaimScoped(
		localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "3344"},
		"run-a", "implementation", time.Hour,
	)
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	service := &Local{sources: LocalSources{
		Layout: layout,
		WorkItemLookup: func(context.Context, string, string) (providers.WorkItem, error) {
			return providers.WorkItem{}, nil
		},
	}}
	runs := []RunSummary{{
		ID: "run-a", Gaggle: "goobers", Phase: journal.PhaseRunning,
		Operator: OperatorRunSummary{
			Issue:             &OperatorIssue{Number: "3344"},
			Claim:             OperatorClaim{LeaseStatus: "none", ProviderMarker: "recorded"},
			PotentialBlockers: []string{},
		},
	}}
	if err := service.decorateOperatorClaims(context.Background(), runs, now); err != nil {
		t.Fatal(err)
	}
	got := runs[0].Operator
	if len(got.PotentialBlockers) != 1 ||
		got.PotentialBlockers[0] != "active claim lease has no provider marker" {
		t.Fatalf("blockers = %+v, want the observed drift", got.PotentialBlockers)
	}
	if len(got.DiagnosticsLimitations) != 0 {
		t.Fatalf("diagnostics = %+v, want none when the reader could see the item", got.DiagnosticsLimitations)
	}
}

func TestParseProviderQuotaResumeTime(t *testing.T) {
	resetAt := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	reason := localscheduler.ReasonProviderQuota + ": resumes at " + resetAt.Format(time.RFC3339)
	got, ok := parseProviderQuotaResumeTime(reason)
	if !ok || !got.Equal(resetAt) {
		t.Fatalf("parseProviderQuotaResumeTime(%q) = %v, %v; want %v, true", reason, got, ok, resetAt)
	}

	for _, invalid := range []string{
		localscheduler.ReasonMaxParallel,
		localscheduler.ReasonBudget,
		"provider-quota",
		localscheduler.ReasonProviderQuota + ": resumes at not-a-time",
	} {
		if _, ok := parseProviderQuotaResumeTime(invalid); ok {
			t.Errorf("parseProviderQuotaResumeTime(%q) = ok, want rejected", invalid)
		}
	}
}

func TestSchedulerStatusProjectsLatestProviderQuotaPause(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	staleReset := time.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC)
	activeReset := staleReset.Add(time.Hour)
	for _, event := range []journal.Event{
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonProviderQuota + ": resumes at " + staleReset.Format(time.RFC3339)},
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonMaxParallel},
		{Type: journal.EventTickSkipped, Reason: localscheduler.ReasonProviderQuota + ": resumes at " + activeReset.Format(time.RFC3339)},
	} {
		if err := log.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.ProviderQuotaResumeAt == nil || !status.ProviderQuotaResumeAt.Equal(activeReset) {
		t.Fatalf("ProviderQuotaResumeAt = %v, want %v", status.ProviderQuotaResumeAt, activeReset)
	}
}

func TestSchedulerStatusProjectsRefillOccupancyAndBlockingCondition(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{
		Type:     journal.EventTickSkipped,
		Gaggle:   "goobers",
		Workflow: "implementation",
		Reason:   "refill blocked: " + localscheduler.ReasonBudget,
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	definitions := testDefinitions()
	definitions.Workflows = []apiv1.Workflow{{
		ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
		Spec: apiv1.WorkflowSpec{
			Gaggle:    "goobers",
			Triggers:  []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:     "stage",
			Tasks:     []apiv1.Task{{Name: "stage", Type: apiv1.TaskDeterministic, Goal: "noop", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
			Readiness: apiv1.ReadinessConditions{DesiredConcurrentRuns: 2, MaxConcurrentRuns: 4},
		},
	}}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: definitions,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.RefillOccupancy) != 1 {
		t.Fatalf("RefillOccupancy = %+v", status.RefillOccupancy)
	}
	occupancy := status.RefillOccupancy[0]
	if occupancy.Gaggle != "goobers" || occupancy.Workflow != "implementation" ||
		occupancy.DesiredRuns != 2 || occupancy.ActiveRuns != 0 ||
		!occupancy.AdmissionBlocked || occupancy.BlockingCondition != localscheduler.ReasonBudget {
		t.Fatalf("occupancy = %+v", occupancy)
	}

	log, _, err = journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{
		Type:     journal.EventTriggerFired,
		Gaggle:   "goobers",
		Workflow: "implementation",
		Reason:   "refill occupancy",
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	status, err = service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.RefillOccupancy[0].AdmissionBlocked {
		t.Fatalf("successful refill attempt retained stale blocker: %+v", status.RefillOccupancy[0])
	}
}

func TestSchedulerStatusProjectsLatestDaemonRestartAndRecoveredRuns(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	machine := fixtureMachine(t)
	startedAt := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	eventTime := startedAt.Add(-time.Hour)
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time {
		return eventTime
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventDaemonStarted}); err != nil {
		t.Fatal(err)
	}
	eventTime = startedAt.Add(-time.Second)
	if err := log.Append(journal.Event{
		Type:   journal.EventDaemonDirtyRestart,
		Reason: "process exited unexpectedly",
	}); err != nil {
		t.Fatal(err)
	}
	eventTime = startedAt
	if err := log.Append(journal.Event{
		Type: journal.EventDaemonStarted,
		Runner: map[string]any{
			"pid":          42,
			"version":      "v1.2.3",
			"instanceRoot": "/srv/goobers",
		},
	}); err != nil {
		t.Fatal(err)
	}
	eventTime = startedAt.Add(time.Second)
	if err := log.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: "run-resumed",
		Runner: map[string]any{
			"kind":   journal.RunnerAnnotationRunRecovery,
			"action": journal.RecoveryActionResumed,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: "run-new",
		Runner: map[string]any{
			"kind":   journal.RunnerAnnotationTriggerRecovery,
			"action": journal.RecoveryActionNewClaim,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	old, oldClock := createFixtureRun(
		t, layout, machine, "run-failed", "implementation", "goobers",
		startedAt.Add(-time.Minute), journal.Trigger{Kind: journal.TriggerItem, Ref: "3090"}, false,
	)
	oldClock.now = startedAt.Add(2 * time.Second)
	if err := old.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"kind":   journal.RunnerAnnotationRunRecovery,
			"reason": "daemon_restart",
			"action": journal.RecoveryActionRetried,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := old.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "implement",
		Attempt: 1,
		Status:  string(apiv1.ResultFailure),
		Error:   &journal.ErrorDetail{Code: "interrupted"},
		Runner:  map[string]any{"interruptedAttempt": true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := old.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, _ := createFixtureRun(
		t, layout, machine, "run-replacement", "implementation", "goobers",
		startedAt.Add(3*time.Second), journal.Trigger{Kind: journal.TriggerItem, Ref: "3090"}, false,
	)
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	genuine, genuineClock := createFixtureRun(
		t, layout, machine, "run-genuine-failure", "implementation", "goobers",
		startedAt.Add(-time.Minute), journal.Trigger{Kind: journal.TriggerItem, Ref: "3091"}, false,
	)
	genuineClock.now = startedAt.Add(2 * time.Second)
	for _, event := range []journal.Event{
		{
			Type: journal.EventRunnerAnnotation,
			Runner: map[string]any{
				"kind": journal.RunnerAnnotationRunRecovery, "reason": "daemon_restart",
				"action": journal.RecoveryActionRetried,
			},
		},
		{
			Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
			Status: string(apiv1.ResultFailure), Error: &journal.ErrorDetail{Code: "interrupted"},
			Runner: map[string]any{"interruptedAttempt": true},
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptInfra},
		{
			Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptInfra,
			Status: string(apiv1.ResultFailure),
			Error:  &journal.ErrorDetail{Code: "external_telemetry_schema_mismatch"},
		},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)},
	} {
		if err := genuine.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	if err := genuine.Close(); err != nil {
		t.Fatal(err)
	}
	genuineReplacement, _ := createFixtureRun(
		t, layout, machine, "run-genuine-replacement", "implementation", "goobers",
		startedAt.Add(3*time.Second), journal.Trigger{Kind: journal.TriggerItem, Ref: "3091"}, false,
	)
	if err := genuineReplacement.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := status.DaemonRestart
	if got == nil ||
		!got.At.Equal(startedAt) ||
		got.Reason != "process exited unexpectedly" ||
		got.PID != 42 ||
		got.Version != "v1.2.3" ||
		got.Root != "/srv/goobers" {
		t.Fatalf("DaemonRestart = %+v", got)
	}
	if len(got.RunIDs) != 1 || got.RunIDs[0] != "run-resumed" {
		t.Fatalf("recovered run IDs = %v, want [run-resumed]", got.RunIDs)
	}
	if len(got.Replacements) != 1 ||
		got.Replacements[0].ItemID != "3090" ||
		got.Replacements[0].FailedRunID != "run-failed" ||
		got.Replacements[0].ReplacementRunID != "run-replacement" {
		t.Fatalf("replacements = %+v", got.Replacements)
	}
}

func TestListStatusRunsSkipsMalformedHistoricalRuns(t *testing.T) {
	service, layout, machine := fixtureService(t)
	startedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	healthy, _ := createFixtureRun(
		t,
		layout,
		machine,
		"healthy-run",
		"implementation",
		"goobers",
		startedAt,
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)
	if err := healthy.Close(); err != nil {
		t.Fatal(err)
	}
	malformed, _ := createFixtureRun(
		t,
		layout,
		machine,
		"malformed-run",
		"implementation",
		"goobers",
		startedAt.Add(-time.Minute),
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)
	if err := malformed.Append(journal.Event{Type: journal.EventRunFinished, Status: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := malformed.Close(); err != nil {
		t.Fatal(err)
	}

	runs, err := service.ListStatusRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "healthy-run" {
		t.Fatalf("ListStatusRuns = %+v, want only healthy-run", runs)
	}
}

func TestListStatusRunsTreatsExecutedTerminalGateAsTerminal(t *testing.T) {
	service, layout, machine := fixtureService(t)
	startedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"terminal-gate-run",
		"implementation",
		"goobers",
		startedAt,
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)
	clock.now = startedAt.Add(time.Minute)
	if err := run.Append(journal.Event{Type: journal.EventGateStarted, Gate: "merge-gate"}); err != nil {
		t.Fatal(err)
	}
	clock.now = startedAt.Add(2 * time.Minute)
	if err := run.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "merge-gate", Verdict: "fail", Target: journal.TargetAbort,
	}); err != nil {
		t.Fatal(err)
	}
	clock.now = startedAt.Add(3 * time.Minute)
	if err := run.Append(journal.Event{Type: journal.EventRefTouched}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	runs, err := service.ListStatusRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListStatusRuns returned %d runs, want 1", len(runs))
	}
	got := runs[0]
	if got.Phase != journal.PhaseAborted || !got.Terminal || got.FinishedAt == nil {
		t.Fatalf("summary = phase %q terminal %v finished %v, want aborted terminal",
			got.Phase, got.Terminal, got.FinishedAt)
	}
	wantFinished := startedAt.Add(2 * time.Minute)
	if !got.FinishedAt.Equal(wantFinished) {
		t.Fatalf("finished_at = %v, want gate time %v", got.FinishedAt, wantFinished)
	}
}

func TestSchedulerStatusRetentionDefaultsAndOptOut(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	cfgDefault := &instance.Config{}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Config:      cfgDefault,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Retention == nil || status.Retention.Window != instance.DefaultProjectionFullFidelityDays {
		t.Fatalf("default retention status = %+v, want window %d", status.Retention, instance.DefaultProjectionFullFidelityDays)
	}

	root := t.TempDir()
	path := filepath.Join(root, instance.ConfigFileName)
	if err := os.WriteFile(path, []byte(`
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
retention:
  projectionFullFidelityDays: 0
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgOptOut, err := instance.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	optOutService, err := NewLocal(LocalSources{
		Layout:      layout,
		Config:      cfgOptOut,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	status, err = optOutService.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Retention == nil || status.Retention.Window != 0 {
		t.Fatalf("opt-out retention status = %+v, want window 0", status.Retention)
	}
}

func TestSchedulerStatusProjectsRetentionLoopDiagnostics(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	lastPassAt := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Config:      &instance.Config{},
		Definitions: testDefinitions(),
		RetentionStats: func() readmodel.RetentionStats {
			return readmodel.RetentionStats{
				Passes:     7,
				AgedOut:    3,
				LastPassAt: lastPassAt,
			}
		},
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Retention == nil ||
		status.Retention.Passes != 7 ||
		status.Retention.AgedOut != 3 ||
		status.Retention.LastPassAt == nil ||
		!status.Retention.LastPassAt.Equal(lastPassAt) {
		t.Fatalf("retention diagnostics = %+v, want passes/agedOut/lastPassAt projected", status.Retention)
	}
}

func writeInitCompleted(t *testing.T, layout instance.Layout, at time.Time) {
	t.Helper()
	instanceLog, _, err := journal.OpenInstanceLog(
		layout.SchedulerDir(),
		journal.WithClock(func() time.Time { return at }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Append(journal.Event{Type: journal.EventInitCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTimeToFirstPRUsesInitCompletionAndOpenedRef(t *testing.T) {
	service, layout, machine := fixtureService(t)
	initCompletedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	writeInitCompleted(t, layout, initCompletedAt)
	first, firstClock := createFixtureRun(
		t, layout, machine, "first-run", "implementation", "goobers",
		initCompletedAt.Add(3*time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	firstClock.now = initCompletedAt.Add(4 * time.Minute)
	if err := first.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: "42"},
		Runner:      map[string]any{"operation": "claim"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, secondClock := createFixtureRun(
		t, layout, machine, "second-run", "implementation", "goobers",
		initCompletedAt.Add(5*time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	secondClock.now = initCompletedAt.Add(8 * time.Minute)
	if err := second.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "7"},
		Runner:      map[string]any{"operation": "update"},
	}); err != nil {
		t.Fatal(err)
	}
	secondClock.now = initCompletedAt.Add(12 * time.Minute)
	if err := second.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "8"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	metric, err := service.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) {
		t.Fatalf("InitCompletedAt = %v, want %v", metric.InitCompletedAt, initCompletedAt)
	}
	wantOpenAt := initCompletedAt.Add(12 * time.Minute)
	if metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(wantOpenAt) {
		t.Fatalf("FirstPROpenAt = %v, want %v", metric.FirstPROpenAt, wantOpenAt)
	}
	if metric.Milliseconds == nil || *metric.Milliseconds != (12*time.Minute).Milliseconds() {
		t.Fatalf("Milliseconds = %v, want %d", metric.Milliseconds, (12 * time.Minute).Milliseconds())
	}
}

func TestTimeToFirstPRIgnoresPersistedPROpenBeforeLegacyInitCompletion(t *testing.T) {
	_, layout, machine := fixtureService(t)
	initCompletedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	legacy, legacyClock := createFixtureRun(
		t, layout, machine, "legacy-run", "implementation", "goobers",
		initCompletedAt.Add(-time.Hour), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	legacyClock.now = initCompletedAt.Add(-30 * time.Minute)
	if err := legacy.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "7"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	legacyDir := legacy.Dir()
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), legacyDir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	writeInitCompleted(t, layout, initCompletedAt)
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	metric, err := service.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) ||
		metric.FirstPROpenAt != nil || metric.Milliseconds != nil {
		t.Fatalf("TimeToFirstPR after legacy no-op init = %#v", metric)
	}

	current, currentClock := createFixtureRun(
		t, layout, machine, "current-run", "implementation", "goobers",
		initCompletedAt.Add(time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	currentClock.now = initCompletedAt.Add(5 * time.Minute)
	if err := current.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "8"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}

	metric, err = service.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(currentClock.now) ||
		metric.Milliseconds == nil || *metric.Milliseconds != (5*time.Minute).Milliseconds() {
		t.Fatalf("TimeToFirstPR after post-init PR = %#v", metric)
	}
}

func TestTimeToFirstPRFailsClosedOnUnreadableJournal(t *testing.T) {
	service, layout, machine := fixtureService(t)
	startedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	healthy, clock := createFixtureRun(
		t, layout, machine, "healthy-run", "implementation", "goobers",
		startedAt, journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock.now = startedAt.Add(12 * time.Minute)
	if err := healthy.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "8"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := healthy.Close(); err != nil {
		t.Fatal(err)
	}

	unreadable, _ := createFixtureRun(
		t, layout, machine, "unreadable-run", "implementation", "goobers",
		startedAt.Add(-time.Hour), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	if err := unreadable.Close(); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(layout.RunsDir(), "unreadable-run", "events.jsonl")
	if err := os.WriteFile(eventsPath, []byte("{]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if metric, err := service.TimeToFirstPR(context.Background()); err == nil {
		t.Fatalf("TimeToFirstPR = %#v, nil; want unreadable journal error", metric)
	}
}

func TestTimeToFirstPRFailsClosedOnUnreadableInstanceJournal(t *testing.T) {
	service, layout, _ := fixtureService(t)
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.SchedulerDir(), "events.jsonl"), []byte("{]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if metric, err := service.TimeToFirstPR(context.Background()); err == nil {
		t.Fatalf("TimeToFirstPR = %#v, nil; want unreadable instance journal error", metric)
	}
}

func TestTimeToFirstPRUsesMilestoneAfterJournalRetention(t *testing.T) {
	_, layout, machine := fixtureService(t)
	initCompletedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	writeInitCompleted(t, layout, initCompletedAt)
	first, _ := createFixtureRun(
		t, layout, machine, "first-run", "implementation", "goobers",
		initCompletedAt.Add(2*time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	firstDir := first.Dir()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, clock := createFixtureRun(
		t, layout, machine, "first-pr-run", "implementation", "goobers",
		initCompletedAt.Add(5*time.Minute), journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock.now = initCompletedAt.Add(12 * time.Minute)
	if err := second.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "8"},
		Runner:      map[string]any{"operation": "open"},
	}); err != nil {
		t.Fatal(err)
	}
	secondDir := second.Dir()
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestSchedulerLog(context.Background(), layout.SchedulerDir()); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}
	for _, dir := range []string{firstDir, secondDir} {
		if err := db.IngestRun(context.Background(), dir); err != nil {
			t.Fatalf("IngestRun(%s): %v", dir, err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	metric, err := service.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) ||
		metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(clock.now) ||
		metric.Milliseconds == nil || *metric.Milliseconds != (12*time.Minute).Milliseconds() {
		t.Fatalf("TimeToFirstPR after retention = %#v", metric)
	}
}

func TestSchedulerStatusPropagatesReadAndContextFailures(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.SchedulerDir(), "events.jsonl"), []byte("{]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SchedulerStatus(context.Background()); err == nil {
		t.Fatal("SchedulerStatus succeeded with a malformed instance journal")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.SchedulerStatus(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("SchedulerStatus canceled error = %v, want context.Canceled", err)
	}
}

// TestSchedulerStatusFoldsIncrementallyWithoutDrift pins the incremental fold
// against the answer a service that read the whole journal would give: the
// bounded read is only safe if a long-lived service and a fresh one project the
// same scheduler state (#3050).
func TestSchedulerStatusFoldsIncrementallyWithoutDrift(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	definitions := testDefinitions()
	definitions.Workflows = []apiv1.Workflow{{
		ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
		Spec: apiv1.WorkflowSpec{
			Gaggle:    "goobers",
			Triggers:  []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:     "stage",
			Tasks:     []apiv1.Task{{Name: "stage", Type: apiv1.TaskDeterministic, Goal: "noop", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
			Readiness: apiv1.ReadinessConditions{DesiredConcurrentRuns: 2, MaxConcurrentRuns: 4},
		},
	}}
	appendSchedulerEvents(t, layout,
		journal.Event{Type: journal.EventDaemonStarted},
		journal.Event{Type: journal.EventWorkflowRefused, Gaggle: "goobers", Workflow: "retired", Reason: "no runner"},
		journal.Event{
			Type:   journal.EventTickSkipped,
			Reason: localscheduler.ReasonProviderQuota + ": resumes at " + time.Date(2026, 8, 20, 4, 0, 0, 0, time.UTC).Format(time.RFC3339),
		},
	)
	service, err := NewLocal(LocalSources{Layout: layout, Definitions: definitions}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SchedulerStatus(context.Background()); err != nil {
		t.Fatal(err)
	}

	appendSchedulerEvents(t, layout,
		journal.Event{Type: journal.EventConfigReloaded},
		journal.Event{Type: journal.EventWorkflowRefused, Gaggle: "goobers", Workflow: "implementation", Reason: "no runner matches"},
		journal.Event{Type: journal.EventDaemonDirtyRestart, Reason: "process exited unexpectedly"},
		journal.Event{Type: journal.EventDaemonStarted},
		journal.Event{Type: journal.EventPollShed, Gaggle: "goobers", Workflow: "implementation"},
	)
	incremental, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	replayed, err := NewLocal(LocalSources{Layout: layout, Definitions: definitions}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	full, err := replayed.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incremental, full) {
		t.Fatalf("incremental fold = %+v, full replay = %+v", incremental, full)
	}
	if len(incremental.RefusedWorkflows) != 0 {
		t.Fatalf("RefusedWorkflows = %+v, want the restart to have cleared them", incremental.RefusedWorkflows)
	}
	if incremental.DaemonRestart == nil || incremental.DaemonRestart.Reason != "process exited unexpectedly" {
		t.Fatalf("DaemonRestart = %+v", incremental.DaemonRestart)
	}
	if len(incremental.RefillOccupancy) != 1 || !incremental.RefillOccupancy[0].AdmissionBlocked ||
		incremental.RefillOccupancy[0].BlockingCondition != localscheduler.ReasonProviderQuota {
		t.Fatalf("RefillOccupancy = %+v", incremental.RefillOccupancy)
	}
}

// TestStatusPathsCostDoesNotGrowWithInstanceJournalHistory is the work fence:
// a repeat scheduler-status or time-to-first-PR request must read a bounded
// journal tail, so ten times the recorded history costs the same request bytes
// rather than ten times as many (#3050).
func TestStatusPathsCostDoesNotGrowWithInstanceJournalHistory(t *testing.T) {
	padding := strings.Repeat("y", 4<<10)
	measure := func(t *testing.T, count int) uint64 {
		t.Helper()
		layout := instance.NewLayout(t.TempDir())
		history := make([]journal.Event, 0, count+1)
		history = append(history, journal.Event{Type: journal.EventInitCompleted})
		for range count {
			history = append(history, journal.Event{Type: journal.EventTickSkipped, Reason: padding})
		}
		appendSchedulerEvents(t, layout, history...)
		service, err := NewLocal(LocalSources{
			Layout:      layout,
			Definitions: testDefinitions(),
		}, func() bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.SchedulerStatus(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := service.TimeToFirstPR(context.Background()); err != nil {
			t.Fatal(err)
		}

		readprobe.Enable()
		t.Cleanup(readprobe.Disable)
		before := readprobe.Take()
		if _, err := service.SchedulerStatus(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := service.TimeToFirstPR(context.Background()); err != nil {
			t.Fatal(err)
		}
		work := readprobe.Take().Sub(before)
		readprobe.Disable()
		if work.InstanceTailReads != 2 {
			t.Fatalf("InstanceTailReads = %d, want one bounded read per request", work.InstanceTailReads)
		}
		return work.InstanceTailBytes
	}

	short := measure(t, 64)
	long := measure(t, 640)
	if short != long {
		t.Fatalf(
			"repeat request read %d journal bytes against a short history and %d against a ten-times-longer one",
			short, long,
		)
	}
}

func TestStatusColdPathReadsInstanceJournalOnce(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	history := make([]journal.Event, 640)
	for i := range history {
		history[i] = journal.Event{Type: journal.EventTickSkipped, Reason: strings.Repeat("y", 4<<10)}
	}
	appendSchedulerEvents(t, layout, history...)
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	readprobe.Enable()
	t.Cleanup(readprobe.Disable)
	if _, err := service.SchedulerStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	work := readprobe.Take()
	if work.InstanceTailReads != 1 || work.InstanceTailRecords != 640 {
		t.Fatalf("cold status work = %+v, want one read and 640 parsed records", work)
	}
}

func appendSchedulerEvents(t *testing.T, layout instance.Layout, events ...journal.Event) {
	t.Helper()
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := log.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}
