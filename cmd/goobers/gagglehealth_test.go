package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gagglehealth"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type gaggleHealthEvidenceFixture struct {
	runs     readservice.RunList
	details  map[string]readservice.RunDetail
	attempts map[string]readservice.AttemptList
	reload   *readservice.DefinitionReloadStatus
}

func (f *gaggleHealthEvidenceFixture) ListRuns(context.Context, readservice.RunListOptions) (readservice.RunList, error) {
	return f.runs, nil
}

func (f *gaggleHealthEvidenceFixture) GetRun(_ context.Context, runID string) (readservice.RunDetail, error) {
	return f.details[runID], nil
}

func (f *gaggleHealthEvidenceFixture) StageAttempts(_ context.Context, runID, stage string) (readservice.AttemptList, error) {
	return f.attempts[runID+"/"+stage], nil
}

func (f *gaggleHealthEvidenceFixture) DefinitionReload() *readservice.DefinitionReloadStatus {
	return f.reload
}

type countingHealthSnapshotSource struct {
	calls atomic.Int32
}

func (s *countingHealthSnapshotSource) Snapshot(context.Context, string, []gagglehealth.EvidenceDependency) (gagglehealth.Snapshot, error) {
	s.calls.Add(1)
	return gagglehealth.Snapshot{}, errors.New("snapshot unavailable")
}

func TestDaemonGaggleHealthSnapshotHonorsBoundedDependencies(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	definitions := &instance.ConfigSet{
		Manifest: &apiv1.Manifest{},
		Gaggles:  []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}}},
		Workflows: []apiv1.Workflow{
			{ObjectMeta: metav1.ObjectMeta{Name: "second"}, Spec: apiv1.WorkflowSpec{Gaggle: "alpha", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "first"}, Spec: apiv1.WorkflowSpec{Gaggle: "alpha", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule}}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "other"}, Spec: apiv1.WorkflowSpec{Gaggle: "beta"}},
		},
	}
	now := time.Now().UTC()
	triggerState := `{"workflows":[{"gaggle":"alpha","workflow":"first","lastEval":"` + now.Format(time.RFC3339Nano) + `"}]}`
	if err := os.WriteFile(filepath.Join(layout.SchedulerDir(), "trigger-evaluations.json"), []byte(triggerState), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := &gaggleHealthEvidenceFixture{reload: &readservice.DefinitionReloadStatus{
		AppliedDigest: "sha256:applied", ObservedDigest: "sha256:observed",
		ObservedAt: now, State: "current",
	}}
	reads.runs.Runs = []readservice.RunSummary{{
		ID: "run-1", Workflow: "first", Gaggle: "alpha", Phase: journal.PhaseEscalated,
		CurrentStage: "implement", LastActivityAt: now,
		ActiveStages: []readmodel.ActiveStage{{Name: "implement", Kind: "stage", Attempt: 2, StartedAt: now}},
		Lineage:      &readservice.RunLineage{WorkspaceBranch: "goobers/run-1", WorkspaceBranchSHA: "abc123"},
	}}
	reads.details = map[string]readservice.RunDetail{"run-1": {
		RunSummary: reads.runs.Runs[0],
		AgentProgress: []readservice.AgentProgressSummary{{
			AgentID: "worker-1", RunID: "run-1", Stage: "implement", Attempt: 2, Worker: true,
			Role: "implementer", Fidelity: "full",
			Current: &readservice.AgentCurrentStatus{Source: "lifecycle", Lifecycle: journal.AgentStarted, UpdatedAt: now},
		}},
	}}
	reads.attempts = map[string]readservice.AttemptList{"run-1/implement": {
		RunID: "run-1", Stage: "implement", Attempts: []readservice.StageAttempt{{
			Number: 2, Status: "running", StartedAt: &now,
			Placement: &journal.Placement{Runner: "self", Host: "daemon", OS: "windows", Worker: "worker-host"},
		}},
	}}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, claimErr := ledger.ClaimScoped(localscheduler.ClaimKey{
		Gaggle: "alpha", Provider: "github", ExternalID: "42",
	}, "run-1", "first", time.Hour); claimErr != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, claimErr)
	}
	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 0, time.Now().Add(time.Hour))
	health := &daemonGaggleHealth{
		root: root, config: &instance.Config{Runners: []instance.RunnerEntry{{
			Name: "self", Host: "self", Provides: instance.RunnerProvides{Capabilities: []string{"git", "go"}},
		}}},
		definitions: definitions, reads: reads, quota: quota,
	}

	snapshot, err := health.Snapshot(context.Background(), "alpha", []gagglehealth.EvidenceDependency{
		gagglehealth.EvidenceWorkflows,
		gagglehealth.EvidenceRuns,
		gagglehealth.EvidenceClaims,
		gagglehealth.EvidenceRunners,
		gagglehealth.EvidenceReconciliation,
		gagglehealth.EvidenceProviders,
		gagglehealth.EvidenceWorkers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workflows) != 2 || snapshot.Workflows[0].ID != "first" ||
		snapshot.Workflows[0].Attributes["triggers"] != "schedule" || snapshot.Workflows[0].UpdatedAt.IsZero() ||
		snapshot.Workflows[1].ID != "second" {
		t.Fatalf("workflows = %v", snapshot.Workflows)
	}
	if len(snapshot.Claims) != 1 || snapshot.Claims[0].ID != "42" ||
		snapshot.Claims[0].Attributes["intervention"] != "required" {
		t.Fatalf("claims = %+v", snapshot.Claims)
	}
	if len(snapshot.Runners) != 2 || snapshot.Runners[0].ID != "self" ||
		snapshot.Runners[0].Attributes["capabilities"] != "git,go" ||
		snapshot.Runners[1].State != "placed" || snapshot.Runners[1].Attributes["worker"] != "worker-host" {
		t.Fatalf("runners = %+v", snapshot.Runners)
	}
	if len(snapshot.Reconciliation) != 1 || snapshot.Reconciliation[0].State != "current" {
		t.Fatalf("reconciliation = %+v", snapshot.Reconciliation)
	}
	if len(snapshot.Providers) != 1 {
		t.Fatalf("providers = %+v", snapshot.Providers)
	}
	if len(snapshot.Workers) != 2 || snapshot.Workers[0].State != "present" ||
		snapshot.Workers[0].Attributes["branch"] != "goobers/run-1" ||
		snapshot.Workers[1].State != string(journal.AgentStarted) {
		t.Fatalf("workers = %+v", snapshot.Workers)
	}

	workflowOnly, err := health.Snapshot(context.Background(), "alpha", []gagglehealth.EvidenceDependency{gagglehealth.EvidenceWorkflows})
	if err != nil {
		t.Fatal(err)
	}
	if len(workflowOnly.Claims) != 0 || len(workflowOnly.Runners) != 0 || len(workflowOnly.Reconciliation) != 0 {
		t.Fatalf("unrequested evidence leaked into snapshot: %+v", workflowOnly)
	}
}

func TestDaemonGaggleHealthRepairTransitionWakesController(t *testing.T) {
	store, err := gagglehealth.OpenStore(t.TempDir(), func(string) (time.Duration, error) {
		return 24 * time.Hour, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := &countingHealthSnapshotSource{}
	controller, err := gagglehealth.NewController(store, source, gagglehealth.ControllerOptions{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background(), []gagglehealth.GaggleRegistration{{
		Name: "alpha", Policy: gagglehealth.DefaultPolicy(),
	}}); err != nil {
		t.Fatal(err)
	}
	defer controller.Stop()
	eventuallyHealth(t, func() bool {
		status, ok := controller.Status("alpha")
		return ok && status.LastError != ""
	})

	health := &daemonGaggleHealth{controller: controller}
	store.SetAppendObserver(health.observeHealthTransition)
	defer store.SetAppendObserver(nil)

	snapshot, err := store.Snapshot("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Active) != 1 {
		t.Fatalf("active findings = %d, want 1", len(snapshot.Active))
	}
	finding := snapshot.Active[0]
	now := time.Now().UTC()
	finding.Repair.Disposition = apiv1.GaggleHealthRepairStarted
	finding.Repair.AttemptedAt = &now
	finding.Repair.IdempotencyKey = "repair-1"
	before := source.calls.Load()
	if _, err := store.AppendNext(apiv1.GaggleHealthEvent{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		OccurredAt:    now, Type: apiv1.GaggleHealthRepairStartedEvent, Gaggle: "alpha",
		EpisodeKey: finding.EpisodeKey, Finding: &finding,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyHealth(t, func() bool { return source.calls.Load() > before })
}

func eventuallyHealth(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}
