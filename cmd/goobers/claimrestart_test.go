package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

type restartClaimPoller struct{ entered chan struct{} }

func (p *restartClaimPoller) PollPullRequest(ctx context.Context, _ providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	p.entered <- struct{}{}
	<-ctx.Done()
	return providers.PullRequestPollResult{}, ctx.Err()
}

func TestUpPreservesLocalClaimAcrossForcedRestart(t *testing.T) {
	root := initDeterministicDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "example")
	t.Setenv("GOOBERS_GITHUB_TOKEN", "test-token")
	t.Setenv("GOOBERS_GITHUB_PR_TOKEN", "test-token")
	l := instance.NewLayout(root)
	wf := strings.Replace(deterministicWorkflowYAML, `      run:
        command: ["true"]`, `      capabilities: ["provider:pr:write"]
      inputs:
        kind: ci-poll
        prNumber: "7"
        pollTimeoutSeconds: "1h"
      run:
        command: ["goobers", "ci-poll"]
        workspace: scratch`, 1)
	if err := os.WriteFile(filepath.Join(l.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml"), []byte(wf), 0600); err != nil {
		t.Fatal(err)
	}
	const runID = "local-claim-restart"
	seedClaimInterruptedRun(t, root, runID)
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "7"}
	seedRestartClaim(t, l, key, runID, time.Now())
	poller := &restartClaimPoller{entered: make(chan struct{}, 8)}
	stubPRPoller(t, poller)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "claimed work", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
	// The first daemon really executes the stage and is forced down mid-call.
	stop := startClaimRestartDaemon(t, root, poller)
	stop()
	dir, err := l.FindRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	if phase, err := reader.Phase(); err != nil || phase != journal.PhaseRunning {
		t.Fatalf("forced stop did not preserve resumable checkpoint: %s %v", phase, err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := ledger.LookupScoped(key); !held || entry.RunID != runID {
		t.Fatalf("forced stop lost claimed ownership: %+v held=%v", entry, held)
	}
	// Model downtime longer than the lease without wall-clock sleeps.
	seedRestartClaim(t, l, key, runID, time.Now().Add(-2*time.Hour))
	stop = startClaimRestartDaemon(t, root, poller)
	defer stop()
	provider := server.newGitHubProvider("token")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	if _, err := reconcileBacklogMetadata(context.Background(), l.ForGaggle("example"), provider, repo, "goobers:approved", defaultBacklogStalenessPolicy(), time.Now); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(server.issueLabels(7), providers.LabelClaimed) {
		t.Fatal("reconciliation stripped live owner's claimed label")
	}
	err = withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationRenewal, func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return err
		}
		entry, held := ledger.LookupScoped(key)
		if !held || entry.RunID != runID || !entry.ExpiresAt.After(time.Now()) {
			t.Errorf("resumed owner missing live lease: %+v held=%v", entry, held)
		}
		if ok, holder, err := ledger.ClaimScoped(key, "competing-run", "default-implement", DefaultClaimLease); err != nil || ok || holder != runID {
			t.Errorf("competing claim: ok=%v holder=%s err=%v", ok, holder, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedRestartClaim(t *testing.T, l instance.Layout, key localscheduler.ClaimKey, runID string, now time.Time) {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName), localscheduler.WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ReleaseScoped(key, runID); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, runID, "default-implement", time.Minute); err != nil || !ok {
		t.Fatalf("seed claim: %v %v", ok, err)
	}
}

func startClaimRestartDaemon(t *testing.T, root string, poller *restartClaimPoller) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	force := make(chan struct{})
	output, diagnostics := newDaemonOutput(), newDaemonOutput()
	done := make(chan int, 1)
	go func() { done <- runUpContextWithForce(ctx, force, []string{root}, output, diagnostics) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		close(force)
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("daemon code=%d stdout=%s stderr=%s", code, output.String(), diagnostics.String())
			}
		case <-time.After(30 * time.Second):
			t.Error("daemon did not stop")
		}
	}
	t.Cleanup(stop)
	select {
	case <-poller.entered:
	case code := <-done:
		stopped = true
		cancel()
		t.Fatalf("daemon exited before stage: %d stdout=%s stderr=%s", code, output.String(), diagnostics.String())
	case <-time.After(30 * time.Second):
		dir, _ := instance.NewLayout(root).FindRunDir("local-claim-restart")
		reader, _ := journal.OpenRead(dir)
		if reader != nil {
			events, _ := reader.Events()
			for _, event := range events {
				if event.Error != nil {
					t.Logf("run error: %+v", event.Error)
				}
			}
		}
		t.Fatalf("stage did not start: stdout=%s stderr=%s", output.String(), diagnostics.String())
	}
	select {
	case <-output.started:
	case <-time.After(30 * time.Second):
		t.Fatalf("startup did not finish: %s %s", output.String(), diagnostics.String())
	}
	return stop
}

func TestStartupLocalClaimGraceDoesNotRenewDeadRunsForever(t *testing.T) {
	l := newClaimTestLayout(t)
	const runID = "abandoned-local-run"
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "removed-workflow", Gaggle: "example", WorkflowVersion: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "7"}
	seedRestartClaim(t, l, key, runID, time.Now().Add(-2*time.Hour))
	steady, closeProbe, err := buildClaimLivenessProbe(&instance.Config{}, nil, func() []string { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer closeProbe()
	gate := localscheduler.NewRecoveryGate()
	if probeErr, err := rebuildClaimRenewalSet(context.Background(), l, localscheduler.CompositeRunLiveness(steady, startupLocalRunLiveness{layout: l}), gate); err != nil || probeErr != nil {
		t.Fatalf("startup renewal: %v %v", err, probeErr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	grace, held := ledger.LookupScoped(key)
	if !held || !grace.ExpiresAt.After(time.Now()) {
		t.Fatalf("startup grace missing: %+v", grace)
	}
	// Workflow was removed, so resume cannot register an executor. The actual
	// periodic probe must not mistake the still-nonterminal journal for life.
	renewed, probeErr, err := renewLiveClaims(context.Background(), l, steady, DefaultClaimLease)
	if err != nil || probeErr != nil || len(renewed) != 0 {
		t.Fatalf("periodic renewed dead run: %+v %v %v", renewed, probeErr, err)
	}
	released, err := recoverClaims(l, nil, grace.ExpiresAt.Add(time.Second), nil, gate)
	if err != nil || len(released) != 1 || released[0].RunID != runID {
		t.Fatalf("dead run did not expire: %+v %v", released, err)
	}
	ledger, err = localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "new-owner", "default-implement", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("dead run still blocks claim: %v %v", ok, err)
	}
}

func TestStartupLocalClaimProbeUsesJournalOwnershipAndTerminalEvents(t *testing.T) {
	for _, tt := range []struct {
		name     string
		driver   journal.RunDriver
		terminal bool
		want     bool
	}{
		{name: "local", want: true}, {name: "engine", driver: journal.DriverEngine}, {name: "terminal", terminal: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := newClaimTestLayout(t)
			jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: "owner", Workflow: "implementation", WorkflowVersion: 1, Driver: tt.driver}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.terminal {
				if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := jr.Close(); err != nil {
				t.Fatal(err)
			}
			live, err := (startupLocalRunLiveness{layout: l}).RunLive(context.Background(), "owner")
			if err != nil || live != tt.want {
				t.Fatalf("live=%v err=%v want=%v", live, err, tt.want)
			}
			for _, missing := range []string{"missing", "owner/backlog-reconcile/1/1"} {
				live, err := (startupLocalRunLiveness{layout: l}).RunLive(context.Background(), missing)
				if live || err != nil {
					t.Fatalf("%s live=%v err=%v", missing, live, err)
				}
			}
		})
	}
}

func seedClaimInterruptedRun(t *testing.T, root, runID string) {
	t.Helper()
	l := instance.NewLayout(root)
	set, report, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatalf("config: %v %+v", err, report)
	}
	for _, wf := range set.Workflows {
		if wf.Name != "default-implement" {
			continue
		}
		m, err := workflow.Compile(workflow.Definition{Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec}, workflow.WithPreviewFeatures(true))
		if err != nil {
			t.Fatal(err)
		}
		pinned, err := json.Marshal(m.Def)
		if err != nil {
			t.Fatal(err)
		}
		jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: wf.Name, WorkflowVersion: 1, WorkflowDigest: m.Digest(), Gaggle: wf.Spec.Gaggle, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: pinned}, journal.WithInputIntegrity(map[string]apiv1.Integrity{journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted}))
		if err != nil {
			t.Fatal(err)
		}
		jr.SetMachineState("local-ci")
		if err := jr.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if err := jr.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("workflow missing")
}

func TestStartupClaimRenewalFailureKeepsRecoveryGateClosed(t *testing.T) {
	l := newClaimTestLayout(t)
	if err := os.WriteFile(filepath.Join(l.SchedulerDir(), claimLedgerFileName), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	gate := localscheduler.NewRecoveryGate()
	_, err := rebuildStartupClaimRenewalSet(context.Background(), l, stubProbe{}, gate)
	if err == nil || gate.RecoveryPermitted() {
		t.Fatalf("renewal err=%v recovery permitted=%v", err, gate.RecoveryPermitted())
	}
}

func TestStartupLocalClaimProbeReportsInvalidEvidence(t *testing.T) {
	l := newClaimTestLayout(t)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: "owner", Workflow: "implementation", WorkflowVersion: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(l.RunsDir(), "owner"), filepath.Join(l.RunsDir(), "wrong-owner")); err != nil {
		t.Fatal(err)
	}
	probe := startupLocalRunLiveness{layout: l}
	if live, err := probe.RunLive(context.Background(), "wrong-owner"); err == nil || live {
		t.Fatalf("identity mismatch live=%v err=%v", live, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := probe.RunLive(ctx, "wrong-owner"); err == nil {
		t.Fatal("cancelled probe succeeded")
	}
}
