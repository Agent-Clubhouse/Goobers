package main

// cloudentrypointwiring_test.go covers the three cloud-mode entry points the
// production instance runs — `goobers worker`, `goobers engine-queues`,
// `goobers engine-project` — at the level their flag-parsing tests do not
// reach: what the handler ASSEMBLES beneath the prologue and hands to the
// runtime. Each handler's own components are tested elsewhere; the assembly
// at the CLI boundary was not, which is the wiring-gap class that produced
// #2965 (#4223).
//
// Same shape as workerwiring_routing_test.go: real production wiring, only
// the transport (Temporal, the cluster) substituted at the existing seams.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/workerhost"
)

// captureWorkerHost substitutes the worker's host seams: the assembled
// workerhost.Config is captured (through the REAL constructor, so its
// validation still runs) and the blocking serve loop is skipped.
func captureWorkerHost(t *testing.T) *workerhost.Config {
	t.Helper()
	var got workerhost.Config
	previousNew, previousRun := newWorkerHost, runWorkerHost
	newWorkerHost = func(cfg workerhost.Config) (*workerhost.Host, error) {
		got = cfg
		return previousNew(cfg)
	}
	runWorkerHost = func(context.Context, *workerhost.Host) error { return nil }
	t.Cleanup(func() { newWorkerHost, runWorkerHost = previousNew, previousRun })
	return &got
}

// declareDispatchRunner gives the instance an engine connection and one
// non-self runner, which is what makes a goobers-dispatch.* queue derivable.
func declareDispatchRunner(t *testing.T, root, runner string) {
	t.Helper()
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Engine = &instance.EngineConfig{
		HostPort: "127.0.0.1:7233", Namespace: "goobers-cloud", TaskQueue: "goobers-engine",
	}
	cfg.Runners = append(cfg.Runners, instance.RunnerEntry{
		Name: runner,
		Host: "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
		Provides: instance.RunnerProvides{
			OS: "linux", CPU: "2000m", Memory: "4Gi", Disk: "20Gi", Shell: true,
		},
	})
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
}

// The worker's mode-1/2 assembly: every operator-supplied root and endpoint
// must reach the constructed runtime, not merely parse. Each assertion below
// is a line whose deletion leaves the worker starting happily and serving the
// wrong thing — a queue nobody dispatches to, a work root nobody claimed, a
// journal that emits nowhere.
func TestRunWorkerWiresResolvedRuntimeIntoTheWorkerHost(t *testing.T) {
	root := initDemo(t)
	workRoot := filepath.Join(t.TempDir(), "work")
	blobRoot := filepath.Join(t.TempDir(), "blobs")
	got := captureWorkerHost(t)

	var stdout, stderr bytes.Buffer
	code := runWorker([]string{
		"--instance", root,
		"--work-root", workRoot,
		"--blob-store", blobRoot,
		"--daemon-api", "http://daemon.goobers.svc:8080",
		"--task-queue", "goobers-engine",
		"--task-queue", "goobers-engine-windows",
		"--temporal-hostport", "temporal.goobers.svc:7233",
		"--temporal-namespace", "goobers-cloud",
		"--drain-timeout", "7s",
		"--config-reload-interval", "0",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr.String())
	}

	if want := []string{"goobers-engine", "goobers-engine-windows"}; !slices.Equal(got.TaskQueues, want) {
		t.Errorf("task queues = %v, want %v", got.TaskQueues, want)
	}
	if got.HostPort != "temporal.goobers.svc:7233" || got.Namespace != "goobers-cloud" {
		t.Errorf("frontend = %s (namespace %s), want the flag values", got.HostPort, got.Namespace)
	}
	if got.DrainTimeout.String() != "7s" {
		t.Errorf("drain timeout = %s, want 7s", got.DrainTimeout)
	}

	// --work-root: the runtime claimed THIS root, so a second worker pointed
	// at it is refused and its stage workspaces land here.
	if _, err := os.Stat(filepath.Join(workRoot, workerRootOwnerFile)); err != nil {
		t.Errorf("worker work root %s not claimed: %v", workRoot, err)
	}
	workspaces, ok := got.Deps.Workspaces.(*workerWorkspaces)
	if !ok {
		t.Fatalf("workspaces = %T, want the instance-credentialed provisioner", got.Deps.Workspaces)
	}
	if want := workerScratchDir(workRoot); workspaces.scratchRoot != want {
		t.Errorf("scratch root = %q, want %q", workspaces.scratchRoot, want)
	}

	// --daemon-api: the live-journal emitter carries the base URL, so a
	// LiveJournal-pinned run's events reach the daemon's journal plane.
	emitter, ok := got.Deps.Journal.(*livejournal.HTTPEmitter)
	if !ok {
		t.Fatalf("journal = %T, want the live-journal HTTP emitter", got.Deps.Journal)
	}
	if emitter.BaseURL != "http://daemon.goobers.svc:8080" {
		t.Errorf("live journal base URL = %q, want the --daemon-api value", emitter.BaseURL)
	}

	// --instance: the agentic/deterministic executors and the dispatch canary
	// are the instance's, not the fail-closed defaults.
	if got.Deps.Goober == nil || got.Deps.Det == nil || got.Deps.Canary == nil {
		t.Errorf("instance seams not wired: goober=%v det=%v canary=%v",
			got.Deps.Goober != nil, got.Deps.Det != nil, got.Deps.Canary != nil)
	}

	// --blob-store: the fleet store is announced by the root it was opened
	// on, which is what makes a multi-worker run's ContextPointers resolvable.
	if !strings.Contains(stdout.String(), blobRoot) {
		t.Errorf("startup output names no artifact store at %s:\n%s", blobRoot, stdout.String())
	}
}

// The mode-3 seam behind --dispatch-namespace: the dispatch queues derived
// from the instance's runner inventory must be SERVED beside the workflow
// queue, and the dispatcher and surrender plane must reach the runtime.
// A worker that parses the flag but serves only its workflow queue leaves
// every dispatched stage unpolled.
func TestRunWorkerServesDerivedDispatchQueuesAndWiresTheDispatcher(t *testing.T) {
	root := initDemo(t)
	declareDispatchRunner(t, root, "linux-pod")
	blobRoot := filepath.Join(t.TempDir(), "blobs")
	got := captureWorkerHost(t)

	previousKube := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return fake.NewClientset(), nil }
	t.Cleanup(func() { dispatchKubeClient = previousKube })
	// The boot-time orphan sweep is the worker's only Temporal contact before
	// it polls; refusing the dial exercises its skip path without a frontend.
	previousDial := dialWorkerSweepTemporal
	dialWorkerSweepTemporal = func(string, string) (client.Client, error) {
		return nil, context.DeadlineExceeded
	}
	t.Cleanup(func() { dialWorkerSweepTemporal = previousDial })

	var stdout, stderr bytes.Buffer
	code := runWorker([]string{
		"--instance", root,
		"--work-root", filepath.Join(t.TempDir(), "work"),
		"--blob-store", blobRoot,
		"--dispatch-namespace", "goobers-stages",
		"--config-reload-interval", "0",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr.String())
	}

	wantQueue := dispatcher.QueuePrefix + ".example.linux-pod"
	if !slices.Contains(got.TaskQueues, wantQueue) {
		t.Errorf("task queues = %v, want the derived dispatch queue %s beside the workflow queue",
			got.TaskQueues, wantQueue)
	}
	if got.TaskQueues[0] != "goobers-engine" {
		t.Errorf("first task queue = %q, want the instance's workflow queue", got.TaskQueues[0])
	}
	if got.Deps.Dispatcher == nil {
		t.Error("stage dispatcher not wired; DispatchStage would fail closed on every mode-3 stage")
	}
	if got.Deps.Surrenders == nil {
		t.Error("surrender plane not wired; a dispatched stage's result would have nowhere to land")
	}
	if !strings.Contains(stdout.String(), "goobers-stages") {
		t.Errorf("startup output does not name the dispatch namespace:\n%s", stdout.String())
	}
}

// --dispatch-namespace without --instance has no runner inventory to derive
// queues from, so it must refuse rather than start a worker that polls
// nothing it was asked to.
func TestRunWorkerDispatchNamespaceRequiresInstance(t *testing.T) {
	t.Setenv("GOOBERS_INSTANCE_ROOT", "")
	got := captureWorkerHost(t)
	var stdout, stderr bytes.Buffer
	code := runWorker([]string{
		"--work-root", filepath.Join(t.TempDir(), "work"),
		"--dispatch-namespace", "goobers-stages",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage error)\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--dispatch-namespace requires --instance") {
		t.Errorf("stderr does not name the requirement:\n%s", stderr.String())
	}
	if len(got.TaskQueues) != 0 {
		t.Error("a worker host was constructed despite the refusal")
	}
}

// fakeEngineQueuesClient is the dialled client engine-queues uses, answered
// from the existing fakeQueueDescriber table.
type fakeEngineQueuesClient struct {
	*fakeQueueDescriber
	closed bool
}

func (c *fakeEngineQueuesClient) Close() { c.closed = true }

// engine-queues is the evidence surface for the queue-ownership check, so the
// queue SET it describes is the whole point: the instance's workflow queue
// plus every goobers-dispatch.* queue the runner inventory implies — derived
// exactly as `goobers worker --dispatch-namespace` derives the queues it
// serves. A command that described only the workflow queue would report a
// clean answer about a set nobody checked.
func TestRunEngineQueuesDescribesTheDerivedQueueSet(t *testing.T) {
	root := initDemo(t)
	declareDispatchRunner(t, root, "linux-pod")
	dispatchQueue := dispatcher.QueuePrefix + ".example.linux-pod"

	describer := &fakeQueueDescriber{pollers: map[string][]string{
		"goobers-engine/workflow":   {"goobers-worker/v1@goobers-worker-0#1"},
		"goobers-engine/activity":   {"goobers-worker/v1@goobers-worker-0#1"},
		dispatchQueue + "/workflow": {"goobers-worker/v1@goobers-worker-0#1"},
		dispatchQueue + "/activity": {"goobers-worker/v1@goobers-worker-0#1"},
	}}
	fakeClient := &fakeEngineQueuesClient{fakeQueueDescriber: describer}
	var dialled client.Options
	previousDial := dialEngineQueues
	dialEngineQueues = func(_ context.Context, opts client.Options) (engineQueuesClient, error) {
		dialled = opts
		return fakeClient, nil
	}
	t.Cleanup(func() { dialEngineQueues = previousDial })

	var stdout, stderr bytes.Buffer
	if code := runEngineQueues([]string{"--json", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr.String())
	}
	if dialled.HostPort != "127.0.0.1:7233" || dialled.Namespace != "goobers-cloud" {
		t.Errorf("dialled %s (namespace %s), want the instance's engine config", dialled.HostPort, dialled.Namespace)
	}
	if !fakeClient.closed {
		t.Error("the dialled client was not closed")
	}

	var rows []queuePollers
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	byKey := map[string]queuePollers{}
	for _, row := range rows {
		byKey[row.Queue+"/"+row.Type] = row
	}
	for _, key := range []string{
		"goobers-engine/workflow", "goobers-engine/activity",
		dispatchQueue + "/workflow", dispatchQueue + "/activity",
	} {
		row, ok := byKey[key]
		if !ok {
			t.Fatalf("no row for %s; described %v", key, describer.asked)
		}
		if len(row.Pollers) != 1 {
			t.Errorf("%s pollers = %v, want the worker's identity", key, row.Pollers)
		}
	}
	if !byKey[dispatchQueue+"/activity"].Dispatch {
		t.Error("the derived dispatch queue is not marked as one")
	}
}

// A describe failure is a reportable finding, not a crash: the command exits
// 1 with the failing queue named.
func TestRunEngineQueuesReportsDescribeFailure(t *testing.T) {
	root := initDemo(t)
	previousDial := dialEngineQueues
	dialEngineQueues = func(context.Context, client.Options) (engineQueuesClient, error) {
		return &fakeEngineQueuesClient{fakeQueueDescriber: &fakeQueueDescriber{err: context.DeadlineExceeded}}, nil
	}
	t.Cleanup(func() { dialEngineQueues = previousDial })

	var stdout, stderr bytes.Buffer
	if code := runEngineQueues([]string{root}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "describe task queue") {
		t.Errorf("stderr does not name the failing describe:\n%s", stderr.String())
	}
}

// fakeProjectionClient records the projection query engine-project issues.
type fakeProjectionClient struct {
	workflowIDs []string
	closed      bool
	err         error
}

func (c *fakeProjectionClient) QueryWorkflow(_ context.Context, workflowID, _, _ string, _ ...interface{}) (converter.EncodedValue, error) {
	c.workflowIDs = append(c.workflowIDs, workflowID)
	return nil, c.err
}

func (c *fakeProjectionClient) Close() { c.closed = true }

// engine-project resolves the frontend from the instance's engine config,
// opens the read-model intake beside it, and projects THE run it was asked
// about into that gaggle's runs directory. The run id reaching the query is
// the wiring: a handler that dropped it would project nothing while exiting
// as if it had.
func TestRunEngineProjectWiresRunIDAndInstanceIntoTheProjection(t *testing.T) {
	root := initDemo(t)
	declareDispatchRunner(t, root, "linux-pod")

	fakeClient := &fakeProjectionClient{err: context.DeadlineExceeded}
	var dialled client.Options
	previousDial := dialEngineProject
	dialEngineProject = func(_ context.Context, opts client.Options) (engineProjectClient, error) {
		dialled = opts
		return fakeClient, nil
	}
	t.Cleanup(func() { dialEngineProject = previousDial })

	var stdout, stderr bytes.Buffer
	code := runEngineProject([]string{"--gaggle", "example", "run-4223", root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (the projection query failed)\nstderr: %s", code, stderr.String())
	}
	if dialled.HostPort != "127.0.0.1:7233" || dialled.Namespace != "goobers-cloud" {
		t.Errorf("dialled %s (namespace %s), want the instance's engine config", dialled.HostPort, dialled.Namespace)
	}
	if !slices.Contains(fakeClient.workflowIDs, "run-4223") {
		t.Errorf("queried workflow ids = %v, want the run id the command was given", fakeClient.workflowIDs)
	}
	if !fakeClient.closed {
		t.Error("the dialled client was not closed")
	}
	if !strings.Contains(stderr.String(), "run-4223") {
		t.Errorf("stderr does not name the run:\n%s", stderr.String())
	}
	// The read-model intake is opened from the instance layout, which is what
	// makes a projected run observable to the read model.
	if _, err := os.Stat(instance.NewLayout(root).IntakeDB()); err != nil {
		t.Errorf("read-model intake not opened under the instance: %v", err)
	}
}

// The dispatch prologue: --gaggle is required, and an unloadable instance is a
// config error, not a dial attempt.
func TestRunEngineProjectUsageAndConfigErrors(t *testing.T) {
	previousDial := dialEngineProject
	dialEngineProject = func(context.Context, client.Options) (engineProjectClient, error) {
		t.Error("temporal was dialled despite a prologue refusal")
		return nil, context.Canceled
	}
	t.Cleanup(func() { dialEngineProject = previousDial })

	cases := []struct {
		name string
		args []string
	}{
		{name: "missing gaggle", args: []string{"run-4223"}},
		{name: "no run id", args: []string{"--gaggle", "example"}},
		{name: "no instance", args: []string{"--gaggle", "example", "run-4223", t.TempDir()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runEngineProject(tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit = %d, want 2\nstderr: %s", code, stderr.String())
			}
		})
	}
}
