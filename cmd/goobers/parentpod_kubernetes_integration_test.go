//go:build integration

package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This qualification replaces only the external model and forge with local
// fixtures. Parent admission, scheduler, MCP tools, queue, driver, Temporal,
// Kubernetes, signed worker APIs, durable wait and return all execute normally.
func TestIntegrationContainedParentAuthorsChildThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	qualifyContainedParentJourney(t, "scratch")
}

func TestIntegrationContainedParentReconcilesChildWorkspaceThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	for _, action := range []string{"merge", "replace", "discard"} {
		t.Run(action, func(t *testing.T) { qualifyContainedParentJourney(t, action) })
	}
}

func TestIntegrationContainedParentIteratesChildrenThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	qualifyContainedParentJourney(t, "iterate")
}

func TestIntegrationContainedParentSurvivesDispatchWorkerRestart(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	qualifyContainedParentJourney(t, "worker-restart")
}

func TestIntegrationContainedParentSurvivesDispatchProcessLoss(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	qualifyContainedParentJourney(t, "worker-crash")
}

func qualifyContainedParentJourney(t *testing.T, action string) {
	t.Helper()
	fenceFirst := action == "cancel-fence-first"
	if fenceFirst {
		action = "cancel"
	}
	parallel := action == "parallel" || action == "parallel-cancel"
	cancelParent := action == "cancel" || action == "parallel-cancel"
	generatedParallel := action == "generated-parallel"
	workerRestart := action == "worker-restart" || action == "worker-crash"
	modelMode := action
	if action == "worker-crash" {
		modelMode = "worker-restart"
	}
	image := os.Getenv("GOOBERS_PARENT_QUALIFICATION_IMAGE")
	if !strings.HasPrefix(image, "localhost:45081/goobers:haw-parent-") {
		t.Fatal("explicit locally built parent qualification image required")
	}
	testdep.Require(t, "git")
	testdep.Require(t, "sh")
	api, namespace := childQualificationKubernetes(t)
	keyPath := filepath.Join(t.TempDir(), "pod.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("q", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	// Host preflight may inspect the CLI, but actual parent inference must
	// execute in the contained worker. Any host invocation fails this probe.
	hostProbe := `#!/bin/sh
case "$*" in
  --version) printf '%s\n' '2.1.0 (qualification preflight)' ;;
  'auth status') printf '%s\n' '{"loggedIn":true}' ;;
  *) printf '%s\n' 'parent execution reached the host' >&2; exit 70 ;;
esac
`
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(hostProbe), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("QUALIFICATION_MODEL_TOKEN", "qualification-model-only")
	f := realParentQualificationFixture(t, image, keyPath, action)
	sourceRepo := t.TempDir()
	recoveryCLIGit(t, sourceRepo, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(sourceRepo, "source.txt"), []byte("parent source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRepo, "qualification-mode"), []byte(modelMode), 0600); err != nil {
		t.Fatal(err)
	}
	var childStarted <-chan struct{}
	if parallel || generatedParallel {
		childStarted = parallelQualificationBarrier(t, sourceRepo)
	} else if cancelParent || workerRestart {
		childStarted = parentQualificationCancellationProbe(t, sourceRepo)
	}
	recoveryCLIGit(t, sourceRepo, "add", ".")
	recoveryCLIGit(t, sourceRepo, "commit", "-m", "qualification base")
	originalClone := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return sourceRepo, nil }
	t.Cleanup(func() { repoCloneURL = originalClone })
	instanceLog, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	registry := newDaemonRunnerRegistry()
	dispatch := newDaemonTriggerService()
	triggers, _, cancels, err := newDaemonCoordinationServices(f.layout, dispatch, registry, instanceLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = triggers.queue.Close(); _ = cancels.receipts.Close() })
	server := httptest.NewUnstartedServer(nil)
	endpoint := "host.docker.internal:" + strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port)
	s := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), instanceLog).withStageGrants(f.layout.Root, endpoint, false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, s) })
	s.childDispatch = dispatch
	s.Replace(credentialPlaneDefinitionsFromSet(f.applied))
	if err := s.enableChildWorkflows(triggers.queue, f.applied); err != nil {
		t.Fatal(err)
	}
	surrenderRoot := t.TempDir()
	plane, err := dispatcher.NewSurrenderDir(surrenderRoot)
	if err != nil {
		t.Fatal(err)
	}
	key, err := podauth.NewSignedKey([]byte(strings.Repeat("q", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := temporaltest.StartDevServer(t.Context(), t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dev.Stop() })
	transport := &observedChildTemporal{Client: dev.Client()}
	journals := childFactoryJournals(t, s)
	observe := childObservationFunc(func(context.Context, httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
		return httpapi.ClaimListResponse{ClaimVisibility: "local", ObservedAt: time.Now()}, nil
	})
	opts := s.installChildPodPlane(transport, plane, journals, observe, nil)
	opts = append(opts, httpapi.WithAuthenticator(auth.WithChildWorkflowGrants(s.grants.key)), httpapi.WithChildWorkflowService(s.children.HTTPService()))
	reads, err := readservice.NewLocal(readservice.LocalSources{Layout: f.layout, Config: f.cfg, Definitions: f.applied, ChildHistory: triggers.queue.Children}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(reads, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	if parallel {
		server.Config.Handler = parallelQualificationHTTPTrace(t, handler)
	}
	server.Start()
	t.Cleanup(server.Close)
	podDispatcher, err := dispatcher.New(dispatcher.Config{InstanceID: f.parent.InstanceID, Owner: "parent-qualification", GaggleNamespaces: map[string]string{"example": namespace}, GaggleServiceAccounts: map[string]string{"example": "qualification-stage"}, EmbeddedVersion: strings.TrimPrefix(image, "localhost:45081/goobers:"), TokenMinter: key, BlobEndpoint: "http://" + endpoint, WriteAPIBase: "http://" + endpoint, SupervisionInterval: 100 * time.Millisecond, LinuxScheduleToStart: 30 * time.Second}, dispatcher.NewKubernetesPodAPI(api), nil, dispatcher.PlaneSurrenderGate{Plane: plane}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: "example", Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := bootstrap.PinStagePlacements(f.cfg, f.applied, "example", machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	queues := map[string]bool{f.cfg.EffectiveEngineConfig().TaskQueue: true}
	for _, pin := range pins {
		queues[pin.Queue] = true
	}
	journeyTimeout := 3 * time.Minute
	if action == "iterate" || parallel || workerRestart {
		// Two child returns require five separate contained parent invocations.
		journeyTimeout = 6 * time.Minute
	}
	ctx, cancel := context.WithTimeout(t.Context(), journeyTimeout)
	defer cancel()
	var workers []temporalworker.Worker
	var process *qualificationHostProcess
	var stopHost context.CancelFunc
	var hostDone chan error
	startWorkers := func() {
		if workerRestart {
			taskQueues := make([]string, 0, len(queues))
			for queue := range queues {
				taskQueues = append(taskQueues, queue)
			}
			if action == "worker-crash" {
				process = startQualificationHostProcess(t, qualificationHostConfig{HostPort: dev.FrontendHostPort(), Queues: taskQueues, InstanceID: f.parent.InstanceID, Namespace: namespace, Image: image, Endpoint: endpoint, KeyPath: keyPath, SurrenderRoot: surrenderRoot})
				return
			}
			host, err := workerhost.New(workerhost.Config{HostPort: dev.FrontendHostPort(), Namespace: "default", TaskQueues: taskQueues, DrainTimeout: time.Second, Deps: bootstrap.EngineDeps{Dispatcher: podDispatcher, Surrenders: plane}})
			if err != nil {
				t.Fatal(err)
			}
			hostCtx, cancel := context.WithCancel(ctx)
			stopHost = cancel
			hostDone = make(chan error, 1)
			done := hostDone
			go func() { done <- host.Run(hostCtx) }()
			return
		}
		for queue := range queues {
			worker := temporalworker.New(dev.Client(), queue, temporalworker.Options{DeadlockDetectionTimeout: temporaltest.DeadlockDetectionTimeout, WorkerStopTimeout: time.Second})
			bootstrap.RegisterEngine(worker, dev.Client(), bootstrap.EngineDeps{Dispatcher: podDispatcher, Surrenders: plane})
			if err := worker.Start(); err != nil {
				t.Fatal(err)
			}
			workers = append(workers, worker)
		}
	}
	stopWorkers := func() {
		if process != nil {
			process.stop(t)
			process = nil
			return
		}
		if stopHost != nil {
			stopHost()
			select {
			case err := <-hostDone:
				if err != nil {
					t.Error("production worker host failed to drain child custody", err)
				}
			case <-ctx.Done():
				t.Error("production worker host did not stop before qualification deadline", ctx.Err())
			}
			stopHost = nil
			return
		}
		for _, worker := range workers {
			worker.Stop()
		}
		workers = nil
	}
	startWorkers()
	t.Cleanup(stopWorkers)
	store, err := executionGenerationStore(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	retainer := &configgeneration.Retainer{Store: store}
	t.Cleanup(func() { _ = retainer.Close() })
	var wg sync.WaitGroup
	build := func(layout instance.Layout, generation string, set *instance.ConfigSet, report *validate.Report) (*schedulerDefinitions, error) {
		return buildSchedulerDefinitions(schedulerDefinitionsInput{Layout: layout, Config: f.cfg, Definitions: set, Validation: report, RunnerRegistry: registry, ProviderQuota: localscheduler.NewProviderQuotaState(), Generations: []*configgeneration.Retainer{retainer}, PinnedGeneration: generation, SharedRegistry: s.shared, InstanceLog: instanceLog, WaitGroup: &wg})
	}
	set, report, err := loadConfigDirectory(f.retainedPath)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := build(f.layout, f.parent.ConfigGeneration, set, report)
	if err != nil {
		t.Fatal(err)
	}
	setup := &schedulerSetup{RunnerRegistry: registry, ChildRuntime: childRuntimeBuilderFor(f.layout, retainer, f.cfg, build)}
	if err := s.installQueuedChildren(setup, triggers, &wg); err != nil {
		t.Fatal(err)
	}

	scheduler := localscheduler.New(definitions.Entries, instanceLog, localscheduler.WithInstanceRunConditions(f.cfg.RunConditions.MaxParallelRuns, f.cfg.RunConditions.WorkflowBudgets, f.cfg.RunConditions.WorkflowDailyBudgets))
	dispatch.AttachScheduler(scheduler)
	dispatch.AttachDispatchContext(ctx)
	t.Cleanup(func() { cancel(); scheduler.Wait(); wg.Wait() })
	accepted, err := triggers.Trigger(ctx, httpapi.TriggerRequest{Gaggle: "example", Workflow: f.parent.Workflow, RequestID: "real-parent-journey", Actor: "qualification-human"})
	if err != nil {
		t.Fatal(err)
	}
	runID := strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
	sawParked, cancellationSent, workerRestarted := false, false, false
	cancelInput := httpapi.CancelRunRequest{RunID: runID, Gaggle: "example", Actor: "qualification-human", IdempotencyKey: "qualification-parent-cancel"}
	var cancelReply httpapi.CancelRunResult
	wantPhase, wantChild, wantParents := journal.PhaseCompleted, triggerqueue.ChildCompleted, 3
	wantChildren := 1
	if action == "iterate" || workerRestart {
		wantChildren, wantParents = 2, 5
	}
	if parallel {
		wantChildren, wantParents = 2, 7
	}
	if cancelParent {
		wantPhase, wantChild, wantParents = journal.PhaseAborted, triggerqueue.ChildCancelled, 1
		if parallel {
			wantParents = 2
		}
	}
	for {
		if err := triggers.Drain(ctx); err != nil {
			t.Fatal("drain parent qualification queue", err)
		}
		if cancelParent && !cancellationSent {
			select {
			case <-childStarted:
				result, err := cancels.Cancel(ctx, cancelInput)
				cancelReply = result
				if err != nil {
					t.Fatal("parent cancellation request failed", result, err)
				}
				if result.Error != "" {
					// Continue observing the physical results so a cleanup
					// failure cannot conceal whether cancellation stopped work.
					t.Errorf("parent cancellation returned an error: %s", result.Error)
				}
				if result.Code != httpapi.CancelCodeRequested || result.Phase != string(journal.PhaseAborted) {
					t.Errorf("cancellation claimed an unsupported outcome: %+v", result)
				}
				cancellationSent = true
				if fenceFirst {
					awaitQualificationFencedChild(t, ctx, registry, triggers.queue, runID)
				}
			default:
			}
		}
		if workerRestart && !workerRestarted {
			select {
			case <-childStarted:
				// Restart the production worker Host while the generated shell
				// is running. Temporal and Kubernetes remain alive; no result,
				// receipt or run state is supplied by this test. The old Host
				// must finish bounded custody settlement and close its client.
				if action == "worker-crash" {
					awaitQualificationChildCustody(t, ctx, transport)
					process.kill(t)
					process = nil
				} else {
					stopWorkers()
				}
				startWorkers()
				workerRestarted = true
			default:
			}
		}
		if dir, err := f.layout.FindRunDir(runID); err == nil {
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			detail, err := reads.GetRun(ctx, runID)
			if err != nil {
				t.Fatal("read parent qualification detail", err)
			}
			if a := detail.ChildActivity; a != nil && a.Status == "recorded" && a.Parked && ((parallel && len(a.Waits) == 2) || (!parallel && len(a.Waits) == 1 && a.Waits[0].Stage == "plan")) {
				sawParked = true
			}
			phase, err := reader.PhaseBounded(ctx)
			if err != nil {
				t.Fatal("read parent qualification phase", err)
			}
			if phase != journal.PhaseRunning {
				if phase != wantPhase {
					events, _ := reader.Events()
					for _, event := range events {
						if event.Error != nil || event.TerminalCause != nil {
							t.Logf("parent %s: error=%+v cause=%+v", event.Type, event.Error, event.TerminalCause)
						}
					}
					t.Fatal("parent did not complete", phase)
				}
				children, childErr := triggers.queue.Children(ctx, triggerqueue.ChildParent{Gaggle: "example", ParentRunID: runID}, "", 10)
				if childErr != nil {
					t.Fatal("read parent qualification children", childErr)
				}
				if len(children) == wantChildren && !slices.ContainsFunc(children, func(c triggerqueue.ChildRecord) bool { return !c.State.Terminal() }) {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("parent journey did not finish", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	children, err := triggers.queue.Children(ctx, triggerqueue.ChildParent{Gaggle: "example", ParentRunID: runID}, "", 10)
	if err != nil || len(children) != wantChildren {
		t.Fatal("child result did not reach the expected retained outcome", children, err)
	}
	for _, child := range children {
		expectedState := wantChild
		if workerRestart && child.Sequence == 1 {
			expectedState = triggerqueue.ChildFailed
		}
		if child.State != expectedState || child.ResultRef == "" || (!cancelParent && child.AcknowledgedAt.IsZero()) {
			logQualificationChildOutcome(t, f.layout, child.RunID)
			t.Fatal("child result did not reach the expected retained outcome", child)
		}
	}
	if action == "iterate" || workerRestart {
		slices.SortFunc(children, func(a, b triggerqueue.ChildRecord) int { return a.Sequence - b.Sequence })
		first, second := children[0], children[1]
		if first.Sequence != 1 || second.Sequence != 2 || first.Identity.StageOccurrence != second.Identity.StageOccurrence || first.Identity.InvocationKey != "qualification-child" || second.Identity.InvocationKey != "qualification-child-2" || first.AcknowledgedAt.After(second.AcceptedAt) {
			t.Fatal("iterative children lost their sequential stage ownership", children)
		}
	}
	if parallel {
		assertParallelQualificationChildren(t, children, childStarted)
	}
	if cancelParent && (!cancellationSent || slices.ContainsFunc(children, func(child triggerqueue.ChildRecord) bool { return !child.CancellationRequested })) {
		t.Fatal("missing authored-child family cancellation")
	}
	if workerRestart && !workerRestarted {
		t.Fatal("dispatch workers were never restarted")
	}
	if !sawParked {
		t.Fatal("Portal run detail never exposed the durable parent wait")
	}
	history, err := reads.RunChildren(ctx, runID, "")
	if err != nil || history.Status != "recorded" || len(history.Items) != wantChildren {
		t.Fatal("Portal child history lost the completed acknowledged child", history, err)
	}
	for _, child := range children {
		found := false
		for _, item := range history.Items {
			if item.RunID == child.RunID && item.State == child.State && (cancelParent || item.AcknowledgedAt != nil) {
				found = true
			}
		}
		if !found {
			t.Fatal("Portal child history lost retained child", child.RunID)
		}
		childDetail, err := reads.GetRun(ctx, child.RunID)
		if err != nil || childDetail.ChildActivity == nil || childDetail.ChildActivity.Parent == nil || childDetail.ChildActivity.Parent.RunID != runID {
			t.Fatal("Portal child detail lost its parent link", childDetail.ChildActivity, err)
		}
	}
	parentDetail, err := reads.GetRun(ctx, runID)
	if err != nil || parentDetail.ChildActivity != nil {
		t.Fatal("Portal retained a cleared parent wait", parentDetail.ChildActivity, err)
	}
	var parents, generated int
	for id, in := range transport.snapshot() {
		var result engine.ChildDispatchResult
		if err := dev.Client().GetWorkflow(ctx, id, "").Get(ctx, &result); err != nil {
			t.Fatal(err)
		}
		if result.BindingDigest != in.BindingDigest() || result.DisposalFailed || result.Report.ChildPodUID == "" || !result.Report.WorkspaceWritersStopped || !result.Report.SurrenderConfirmed {
			t.Fatal("unverified physical custody", result)
		}
		if in.Attempt.WorkflowParent {
			parents++
		} else {
			generated++
		}
	}
	wantGenerated := wantChildren
	if generatedParallel {
		wantGenerated = 3
		select {
		case <-childStarted:
		default:
			t.Fatal("generated child branches did not overlap")
		}
	}
	if parents != wantParents || generated != wantGenerated {
		t.Fatal("unexpected parent/child physical invocations", parents, generated)
	}
	if cancelParent {
		if parallel {
			assertCancelledParallelParentsRetained(t, f, runID, children)
		} else {
			assertCancelledParentRetained(t, f, runID, children[0])
		}
		replay, err := cancels.Cancel(ctx, cancelInput)
		if err != nil || replay != cancelReply {
			t.Fatal("durable cancellation replay changed its original acceptance", replay, err)
		}
	}
}

func realParentQualificationFixture(t *testing.T, image, keyPath string, mode ...string) pinnedChildFixture {
	t.Helper()
	return newPinnedChildFixture(t, func(root string) {
		parent := strings.Replace(childValidationParent, "      goal:", "      workspace: repo\n      runsOn: {os: linux, capabilities: [isolated-parent]}\n      goal:", 1)
		parent = strings.Replace(parent, "allowPRPublication: true", "allowPRPublication: false", 1)
		parallel := len(mode) != 0 && (mode[0] == "parallel" || mode[0] == "parallel-cancel")
		if parallel {
			parent = parallelQualificationDefinition(t, parent)
		}
		writeFileContent(t, filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml"), parent)
		path := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
		writeFileContent(t, path, strings.Replace(readFileContent(t, path), "harness: copilot", "harness: claude-code", 1))
		path = filepath.Join(root, "instance.yaml")
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(readFileContent(t, path)), &doc); err != nil {
			t.Fatal(err)
		}
		if parallel {
			conditions, _ := doc["runConditions"].(map[string]any)
			if conditions == nil {
				conditions = map[string]any{}
			}
			conditions["maxParallelRuns"] = 2
			doc["runConditions"] = conditions
		}
		delete(doc, "runner")
		doc["schemaVersion"] = 2
		grants, _ := doc["credentials"].([]any)
		doc["credentials"] = append(grants, map[string]any{"capability": "agent:model", "harness": "claude-code", "token": map[string]any{"env": "QUALIFICATION_MODEL_TOKEN"}})
		doc["engine"] = map[string]any{"hostPort": "temporal:7233"}
		doc["api"] = map[string]any{"podTokenKeyFile": keyPath}
		doc["runners"] = []any{map[string]any{"name": "self", "host": "self"}, map[string]any{"name": "isolated", "host": image, "provides": map[string]any{"os": "linux", "harnesses": []string{"claude-code", "claude"}, "shell": true, "capabilities": []string{"isolated-parent", "isolated-child"}}}}
		data, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		writeFileContent(t, path, string(data))
	})
}
