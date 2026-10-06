package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gagglehealth"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
			{ObjectMeta: metav1.ObjectMeta{Name: "second"}, Spec: apiv1.WorkflowSpec{Gaggle: "alpha"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "first"}, Spec: apiv1.WorkflowSpec{Gaggle: "alpha"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "other"}, Spec: apiv1.WorkflowSpec{Gaggle: "beta"}},
		},
	}
	reads, err := readservice.NewLocal(readservice.LocalSources{
		Layout: layout, Definitions: &instance.ConfigSet{Manifest: &apiv1.Manifest{}},
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	reads.PublishDefinitionReload(readservice.DefinitionReloadStatus{
		AppliedDigest: "sha256:applied", ObservedDigest: "sha256:observed",
		ObservedAt: time.Now().UTC(), State: "current",
	})
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
		root: root, config: &instance.Config{Runners: []instance.RunnerEntry{{Name: "self", Host: "self"}}},
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
	if len(snapshot.Workflows) != 2 || snapshot.Workflows[0] != "first" || snapshot.Workflows[1] != "second" {
		t.Fatalf("workflows = %v", snapshot.Workflows)
	}
	if len(snapshot.Claims) != 1 || snapshot.Claims[0].ID != "42" {
		t.Fatalf("claims = %+v", snapshot.Claims)
	}
	if len(snapshot.Runners) != 1 || snapshot.Runners[0].ID != "self" {
		t.Fatalf("runners = %+v", snapshot.Runners)
	}
	if len(snapshot.Reconciliation) != 1 || snapshot.Reconciliation[0].State != "current" {
		t.Fatalf("reconciliation = %+v", snapshot.Reconciliation)
	}
	if len(snapshot.Providers) != 1 {
		t.Fatalf("providers = %+v", snapshot.Providers)
	}

	workflowOnly, err := health.Snapshot(context.Background(), "alpha", []gagglehealth.EvidenceDependency{gagglehealth.EvidenceWorkflows})
	if err != nil {
		t.Fatal(err)
	}
	if len(workflowOnly.Claims) != 0 || len(workflowOnly.Runners) != 0 || len(workflowOnly.Reconciliation) != 0 {
		t.Fatalf("unrequested evidence leaked into snapshot: %+v", workflowOnly)
	}
}
