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
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This qualification replaces only the external model and forge with local
// fixtures. Parent admission, scheduler, MCP tools, queue, driver, Temporal,
// Kubernetes, signed worker APIs, durable wait and return all execute normally.
func TestIntegrationContainedParentAuthorsChildThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	image := os.Getenv("GOOBERS_PARENT_QUALIFICATION_IMAGE")
	if !strings.HasPrefix(image, "localhost:45081/goobers:haw-parent-") {
		t.Fatal("explicit locally built parent qualification image required")
	}
	testdep.Require(t, "git")
	testdep.Require(t, "node")
	api, namespace := childQualificationKubernetes(t)
	keyPath := filepath.Join(t.TempDir(), "pod.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("q", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	model, err := os.ReadFile("testdata/qualification-claude.cjs")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), model, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("QUALIFICATION_MODEL_TOKEN", "qualification-model-only")
	f := realParentQualificationFixture(t, image, keyPath)
	sourceRepo := t.TempDir()
	recoveryCLIGit(t, sourceRepo, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(sourceRepo, "source.txt"), []byte("parent source\n"), 0600); err != nil {
		t.Fatal(err)
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
	triggers, _, _, err := newDaemonCoordinationServices(f.layout, dispatch, registry, instanceLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = triggers.queue.Close() })
	server := httptest.NewUnstartedServer(nil)
	endpoint := "host.docker.internal:" + strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port)
	s := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), instanceLog).withStageGrants(f.layout.Root, endpoint, false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, s) })
	if err := s.enableChildWorkflows(triggers.queue, f.applied); err != nil {
		t.Fatal(err)
	}
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
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
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
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
	for queue := range queues {
		worker := temporalworker.New(dev.Client(), queue, temporalworker.Options{DeadlockDetectionTimeout: temporaltest.DeadlockDetectionTimeout, WorkerStopTimeout: time.Second})
		engine.RegisterWith(worker, &engine.Activities{Dispatcher: podDispatcher, Surrenders: plane})
		if err := worker.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(worker.Stop)
	}
	store, err := executionGenerationStore(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	retainer := &configgeneration.Retainer{Store: store}
	t.Cleanup(func() { _ = retainer.Close() })
	var wg sync.WaitGroup
	build := func(layout instance.Layout, generation string, set *instance.ConfigSet, report *validate.Report) (*schedulerDefinitions, error) {
		return buildSchedulerDefinitions(schedulerDefinitionsInput{Layout: layout, Config: f.cfg, Definitions: set, Validation: report, RunnerRegistry: registry, ProviderQuota: localscheduler.NewProviderQuotaState(), Generations: []*configgeneration.Retainer{retainer}, PinnedGeneration: generation, InstanceLog: instanceLog, WaitGroup: &wg})
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
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	scheduler := localscheduler.New(definitions.Entries, instanceLog)
	dispatch.AttachScheduler(scheduler)
	dispatch.AttachDispatchContext(ctx)
	t.Cleanup(func() { cancel(); scheduler.Wait(); wg.Wait() })
	accepted, err := triggers.Trigger(ctx, httpapi.TriggerRequest{Gaggle: "example", Workflow: f.parent.Workflow, RequestID: "real-parent-journey", Actor: "qualification-human"})
	if err != nil {
		t.Fatal(err)
	}
	runID := strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
	for {
		if err := triggers.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if dir, err := f.layout.FindRunDir(runID); err == nil {
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			phase, err := reader.PhaseBounded(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if phase != journal.PhaseRunning {
				if phase != journal.PhaseCompleted {
					events, _ := reader.Events()
					for _, event := range events {
						if event.Error != nil || event.TerminalCause != nil {
							t.Logf("parent %s: error=%+v cause=%+v", event.Type, event.Error, event.TerminalCause)
						}
					}
					t.Fatal("parent did not complete", phase)
				}
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("parent journey did not finish", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	children, err := triggers.queue.Children(ctx, triggerqueue.ChildParent{Gaggle: "example", ParentRunID: runID}, "", 10)
	if err != nil || len(children) != 1 || children[0].State != triggerqueue.ChildCompleted || children[0].AcknowledgedAt.IsZero() {
		t.Fatal("child result was not completed and acknowledged", children, err)
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
	if parents != 3 || generated != 1 {
		t.Fatal("unexpected parent/child physical invocations", parents, generated)
	}
}

func realParentQualificationFixture(t *testing.T, image, keyPath string) pinnedChildFixture {
	t.Helper()
	return newPinnedChildFixture(t, func(root string) {
		parent := strings.Replace(childValidationParent, "      goal:", "      workspace: repo\n      runsOn: {os: linux, capabilities: [isolated-parent]}\n      goal:", 1)
		parent = strings.Replace(parent, "allowPRPublication: true", "allowPRPublication: false", 1)
		writeFileContent(t, filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml"), parent)
		path := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
		writeFileContent(t, path, strings.Replace(readFileContent(t, path), "harness: copilot", "harness: claude-code", 1))
		path = filepath.Join(root, "instance.yaml")
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(readFileContent(t, path)), &doc); err != nil {
			t.Fatal(err)
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
