//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryPublicationTakesVerifiedHostCustody(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{
		"live",
		"cold-pending-times-out-before-terminal-release",
		"warm-ready-finishes-before-terminal-release",
		"successor-owns-before-late-publication",
		"finished-during-transfer",
		"crashed-reservation-present",
	} {
		t.Run(mode, func(t *testing.T) {
			testRecoveryPublicationCustody(t, mode)
		})
	}
}

func testRecoveryPublicationCustody(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderKind(cfg.Repos[0].Provider), Owner: cfg.Repos[0].Owner, Name: cfg.Repos[0].Name}
	const runID = "publication-worker"
	started := time.Now().UTC().Add(-60 * 24 * time.Hour)
	finished := time.Now().UTC().Add(-time.Hour)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: started}, nil, journal.WithClock(func() time.Time { return finished }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("7", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim: %t %v", ok, err)
	}
	seedItemRepositoryForTest(t, layout, runID, "7", repo)
	if mode == "cold-pending-times-out-before-terminal-release" {
		runHeartbeatTimeoutLifecycle(t, layout, run, runID, false)
	}
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	manager, err := worktree.NewManager(t.TempDir(), worktree.WithRemoteGitGate(func(context.Context, string) error {
		t.Fatal("archive publication attempted a forge operation")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	previous := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = previous })
	// Seed the same-keyed managed mirror directly with plain git, matching
	// what a live run's own workspace provisioning already establishes by
	// the time any run exists (WorkingCopy populates this mirror, and the
	// run's `git worktree add` workspace shares its object storage) — so the
	// base commit below is already present when recovery captures it. This
	// must not go through manager.WorkingCopy itself: this test asserts
	// archive publication performs no forge/remote git operation at all.
	mirrorDir := filepath.Join(manager.Root, worktree.RepositoryDigest(source)[:16], "repo.git")
	if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o755); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, t.TempDir(), "clone", "--mirror", source, mirrorDir)
	if err := os.WriteFile(filepath.Join(source, "implementation.txt"), []byte("remote worker changes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately disagree with the host clock and retention policy.
	prepared, err := recovery.PrepareRecord(ctx, source, repo.CanonicalKey(), runID, "main", started.Add(time.Minute), started.Add(365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	archive := t.TempDir()
	record, err := recovery.PublishRetainedState(ctx, source, archive, []string{source}, prepared, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := recovery.WriteArchiveEnvelope(ctx, filepath.Join(archive, recovery.BundleFileName), record, 1<<20, &wire); err != nil {
		t.Fatal(err)
	}
	service := recoveryDeliveryService{layout: layout, setup: &schedulerSetup{LegacyWorktrees: manager}}
	var body io.Reader = bytes.NewReader(wire.Bytes())
	if mode == "warm-ready-finishes-before-terminal-release" {
		runHeartbeatTimeoutLifecycle(t, layout, run, runID, true)
	}
	if mode == "successor-owns-before-late-publication" {
		if err := ledger.Release("7", runID, claimrelease.Completed.Reason()); err != nil {
			t.Fatal(err)
		}
		if ok, _, err := ledger.Claim("7", "successor-run", "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("successor claim: %t %v", ok, err)
		}
	}
	if mode == "finished-during-transfer" {
		body = &recoveryPublicationHookReader{Reader: body, beforeRead: func() error {
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
				return err
			}
			// Terminal renewal scans an empty inventory while the archive is
			// still on the wire. Intake must close this ordering gap itself.
			return renewTerminalRecovery(layout, manager, runID)
		}}
	}
	if mode == "crashed-reservation-present" {
		// An earlier publish killed mid-transfer must not wedge this one.
		seedRecoveryDebris(t, layout, "0000000000000000-deadbeef")
	}
	err = service.PublishRecovery(ctx, runID, repo.CanonicalKey(), "7", body)
	wantRefusal := mode == "successor-owns-before-late-publication"
	if (err != nil) != wantRefusal {
		t.Fatalf("late publication refusal=%t: %v", wantRefusal, err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	captures, err := recovery.RecordsFromEvents(events, runID)
	if err != nil {
		t.Fatal(err)
	}
	if wantRefusal {
		if len(captures) != 0 {
			t.Fatalf("successor-fenced publication acknowledged: %+v", captures)
		}
		return
	}
	deadline := started.Add(30 * 24 * time.Hour)
	if mode == "finished-during-transfer" || mode == "cold-pending-times-out-before-terminal-release" || mode == "warm-ready-finishes-before-terminal-release" {
		deadline = finished.Add(30 * 24 * time.Hour)
	}
	if len(captures) != 1 || !captures[0].CreatedAt.Equal(started) || !captures[0].RetainUntil.Equal(deadline) {
		t.Fatalf("host custody policy: %+v", captures)
	}
	found, err := manager.WithExistingMirror(ctx, source, func(mirror string) error {
		if got := recoveryCLIGit(t, mirror, "show", captures[0].Ref+":implementation.txt"); got != "remote worker changes" {
			t.Fatalf("host ref content: %q", got)
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("missing managed custody repository: found=%t err=%v", found, err)
	}
	if mode == "crashed-reservation-present" {
		return
	}
	verifyRecoveryPublicationArchive(t, service, wire.Bytes(), runID, repo.CanonicalKey(), deadline, source)
}

type heartbeatLifecycleRelay struct {
	cancel context.CancelFunc
	cold   bool
	ready  bool
}

func (r *heartbeatLifecycleRelay) RelayLiveness(_ context.Context, _ dispatcher.Attempt, _ string, phase corev1.PodPhase) error {
	if phase == corev1.PodRunning {
		r.ready = true
	}
	if r.cold && phase == corev1.PodPending {
		r.cancel()
	}
	return nil
}

type heartbeatLifecycleGate struct {
	cancel    context.CancelFunc
	completed bool
}

func (g *heartbeatLifecycleGate) Confirmed(ctx context.Context, _ dispatcher.Attempt) (bool, error) {
	g.completed = true
	g.cancel()
	<-ctx.Done()
	return false, ctx.Err()
}

func runHeartbeatTimeoutLifecycle(t *testing.T, layout instance.Layout, run *journal.Run, runID string, warm bool) {
	t.Helper()
	api := fake.NewSimpleClientset()
	var pod *corev1.Pod
	var observations int
	var deleted bool
	api.PrependReactor("create", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
		pod = action.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = types.UID("uid-" + pod.Name)
		pod.Status.Phase = corev1.PodPending
		observations = 0
		deleted = false
		return false, nil, nil
	})
	api.PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		if pod == nil || deleted {
			return false, nil, nil
		}
		observations++
		observed := pod.DeepCopy()
		if warm {
			if observations == 1 {
				observed.Status.Phase = corev1.PodRunning
				observed.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			} else {
				observed.Status.Phase = corev1.PodSucceeded
			}
		}
		return true, observed, nil
	})
	api.PrependReactor("delete", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return false, nil, nil
	})
	relay := &heartbeatLifecycleRelay{cold: !warm}
	gate := &heartbeatLifecycleGate{}
	stageDispatcher, err := dispatcher.New(dispatcher.Config{
		GaggleNamespaces:    map[string]string{"goobers": "test"},
		EmbeddedVersion:     "v1",
		SupervisionInterval: time.Millisecond,
	}, dispatcher.NewKubernetesPodAPI(api), relay, gate, nil)
	if err != nil {
		t.Fatal(err)
	}
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	activities := &engine.Activities{Dispatcher: stageDispatcher, Surrenders: plane}
	timeout := temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil)
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(activities)
	env.OnActivity(engine.ActDispatchStage, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, input engine.DispatchStageInput) (engine.DispatchStageResult, error) {
			dispatchCtx, cancel := context.WithCancel(ctx)
			relay.cancel = cancel
			gate.cancel = cancel
			_, dispatchErr := activities.DispatchStage(dispatchCtx, input)
			if dispatchErr == nil || !strings.Contains(dispatchErr.Error(), "control path lost") {
				return engine.DispatchStageResult{}, fmt.Errorf("dispatch interruption = %v, want lost heartbeat control path", dispatchErr)
			}
			return engine.DispatchStageResult{}, timeout
		},
	)
	env.ExecuteWorkflow(engine.Run, engine.RunInput{
		RunID: runID, Gaggle: "goobers", WorkflowName: "implementation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers", Start: "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
				Run: &apiv1.DeterministicRun{Command: []string{"go", "version"}, Workspace: apiv1.WorkspaceScratch},
			}},
		},
		Placements: []engine.PinnedPlacement{{
			Stage: "implement", Queue: dispatcher.QueueName("goobers", "windows"),
			Eligible: []dispatcher.RunnerSpec{{
				Name: "windows", OS: "windows", HostKind: "image", Host: "example/windows:v1",
			}},
		}},
	})
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(strings.ToLower(err.Error()), "heartbeat") {
		t.Fatalf("workflow heartbeat timeout = %v", err)
	}
	if warm && (!relay.ready || !gate.completed) {
		t.Fatalf("warm lifecycle ready=%t agentCompleted=%t, want both before timeout", relay.ready, gate.completed)
	}
	if !warm && (relay.ready || gate.completed) {
		t.Fatalf("cold lifecycle ready=%t agentCompleted=%t, want timeout while Pending", relay.ready, gate.completed)
	}
	// Production terminal hooks run only after the workflow's durable finish.
	// Preserve that order so claim release records Failed rather than Unknown.
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	hooks := &engineTerminalHooks{
		finalize: func(id string, _ journal.RunPhase) error {
			return releaseClaimsForRun(layout, nil, id)
		},
	}
	hooks.run(context.Background(), engineTerminalOutcome{
		RunID: runID, Gaggle: "goobers", Workflow: "implementation",
		Phase: journal.PhaseFailed, Err: env.GetWorkflowError(),
	})
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if held := ledger.ForRunAll(runID); len(held) != 0 {
		t.Fatalf("terminal workflow retained claims: %+v", held)
	}
	if released := ledger.HistoryForRun(runID); len(released) != 1 || released[0].ReleaseReason.Disposition != claimrelease.Failed {
		t.Fatalf("terminal claim release did not preserve failed outcome: %+v", released)
	}
}

func verifyRecoveryPublicationArchive(t *testing.T, service recoveryDeliveryService, wire []byte, runID, key string, deadline time.Time, base string) {
	t.Helper()
	ctx := context.Background()
	publishRecoveryOverAuthenticatedHTTP(t, service, wire, runID)
	entries, err := recovery.ReadInventory(ctx, filepath.Join(service.layout.Root, "recovery"), 128)
	if err != nil || len(entries) != 1 {
		t.Fatalf("retry inventory: %+v %v", entries, err)
	}
	entry := entries[0]
	entry.Record, err = recovery.ReadRetainedRecord(entry.RecordPath)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Record.RetainUntil.Equal(deadline) {
		t.Fatalf("retry changed durable retention deadline: got=%s want=%s", entry.Record.RetainUntil, deadline)
	}
	destination := t.TempDir()
	recoveryCLIGit(t, destination, "init", "--bare")
	// An independent host that already tracks the base branch, the same
	// property a real managed mirror has, is what makes a delta bundle
	// restorable here.
	recoveryCLIGit(t, destination, "fetch", base, "main")
	if err := recovery.ImportSnapshotBundle(ctx, destination, filepath.Join(filepath.Dir(entry.RecordPath), recovery.BundleFileName), entry.Record, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got := recoveryCLIGit(t, destination, "show", entry.Record.Ref+":implementation.txt"); got != "remote worker changes" {
		t.Fatalf("independent host archive content: %q", got)
	}
}

func publishRecoveryOverAuthenticatedHTTP(t *testing.T, service recoveryDeliveryService, wire []byte, runID string) {
	t.Helper()
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator), httpapi.WithRecoveryService(service))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	archive := t.TempDir()
	record, err := recovery.ReceiveArchiveEnvelope(context.Background(), bytes.NewReader(wire), archive, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{podauth.ScopeJournal, podauth.ScopeClaims, "runtime"} {
		var token string
		if scope == "runtime" {
			// The dispatch parent already has this run-scoped runtime token;
			// it is never passed through to the stage subprocess.
			token, err = registry.Mint(runID, time.Hour)
		} else {
			token, err = registry.MintScoped(runID, time.Hour, scope)
		}
		if err != nil {
			t.Fatal(err)
		}
		publisher := recovery.HTTPArchivePublisher{BaseURL: server.URL, Token: token, RunID: runID}
		err = publisher.PublishArchive(context.Background(), "7", record, filepath.Join(archive, recovery.BundleFileName))
		if (err != nil) != (scope == podauth.ScopeJournal) {
			t.Fatalf("authenticated %s upload: %v", scope, err)
		}
	}
}

type recoveryPublicationHookReader struct {
	io.Reader
	beforeRead func() error
}

func (r *recoveryPublicationHookReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		beforeRead := r.beforeRead
		r.beforeRead = nil
		if err := beforeRead(); err != nil {
			return 0, err
		}
	}
	return r.Reader.Read(p)
}
