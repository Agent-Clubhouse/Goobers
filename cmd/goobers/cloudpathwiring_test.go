package main

// cloudpathwiring_test.go covers the three CLI entry points the cloud
// instance actually runs — `goobers worker`, `goobers engine-queues`,
// `goobers engine-project` — BELOW their flag-parsing prologues (#4223).
//
// Every one of them resolves something no other code resolves: which frontend
// and namespace to speak to, which queue set to serve or describe, which
// directories back the workspaces and the artifact/surrender planes, and
// where live journal events go. That resolution is unobservable once it has
// been handed to a constructed Temporal client or worker host, so each test
// substitutes the process-boundary seam (newWorkerHost, dialEngineTemporal,
// dispatchKubeClient) and asserts the values the CLI actually assembled — the
// wiring-gap class of #2965, where individually-tested components are
// assembled wrongly at the CLI boundary and the mistake surfaces first in
// production.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/workerhost"
)

// fakeWorkerHost is the constructed worker host, minus Temporal: Run returns
// what the test wants the drain to be.
type fakeWorkerHost struct {
	err error
	ran bool
}

func (h *fakeWorkerHost) Run(context.Context) error {
	h.ran = true
	return h.err
}

// withFakeWorkerHost substitutes the worker-host seam and returns a pointer to
// the config runWorker assembled, filled by the time runWorker returns.
func withFakeWorkerHost(t *testing.T, runErr error) (*workerhost.Config, *fakeWorkerHost) {
	t.Helper()
	var built workerhost.Config
	host := &fakeWorkerHost{err: runErr}
	previous := newWorkerHost
	newWorkerHost = func(cfg workerhost.Config) (workerHostServer, error) {
		built = cfg
		return host, nil
	}
	t.Cleanup(func() { newWorkerHost = previous })
	return &built, host
}

// withNeutralWorkerEnv clears the fleet-wide env vars the worker reads as flag
// defaults, so an ambient value in the CI process cannot decide which shape
// these tests exercise.
func withNeutralWorkerEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GOOBERS_INSTANCE_ROOT", "GOOBERS_BLOB_STORE", "GOOBERS_DAEMON_API",
		"GOOBERS_DISPATCH_NAMESPACE", "GOOBERS_POD_TOKEN",
	} {
		t.Setenv(key, "")
	}
}

// cloudInstance is an instance declaring an engine connection and one non-self
// runner — the minimum shape `--dispatch-namespace` has anything to serve on.
func cloudInstance(t *testing.T) string {
	t.Helper()
	root := initDemo(t)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Engine = &instance.EngineConfig{
		HostPort: "temporal.svc:7233", Namespace: "goobers", TaskQueue: "goobers-engine",
	}
	cfg.Runners = append(cfg.Runners, instance.RunnerEntry{
		Name: "linux-pod",
		Host: "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
		Provides: instance.RunnerProvides{
			OS: "linux", CPU: "2000m", Memory: "4Gi", Disk: "20Gi", Shell: true,
		},
	})
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	return root
}

// The mode-3 cloud shape, end to end through runWorker: every value the
// operator passed must reach the constructed runtime, not merely parse. Each
// assertion below is a wire that is silent when cut — a dropped --daemon-api
// base URL leaves live journal emission off, a dispatch queue missing from
// the served set leaves stages nobody polls for, a work root that never
// reaches the provisioner puts workspaces somewhere the operator did not
// mount.
func TestRunWorkerWiresTheResolvedCloudPathIntoTheWorkerHost(t *testing.T) {
	withNeutralWorkerEnv(t)
	root := cloudInstance(t)
	workRoot := filepath.Join(t.TempDir(), "work")
	blobRoot := filepath.Join(t.TempDir(), "blobs")
	const daemonAPI = "http://goobers-api.goobers.svc:8080"

	previousKube := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return fake.NewClientset(), nil }
	t.Cleanup(func() { dispatchKubeClient = previousKube })

	var dispatch dispatcher.Config
	previousDispatcher := newStageDispatcher
	newStageDispatcher = func(c dispatcher.Config, pods dispatcher.PodAPI, relay dispatcher.JournalRelay, gate dispatcher.SurrenderGate, capacity dispatcher.CapacityProber) (*dispatcher.Dispatcher, error) {
		dispatch = c
		return previousDispatcher(c, pods, relay, gate, capacity)
	}
	t.Cleanup(func() { newStageDispatcher = previousDispatcher })

	withFakeSweepDial(t, &fakeSweepDescriber{})
	built, host := withFakeWorkerHost(t, nil)

	code, stdout, stderr := runArgs(t, "worker",
		"--instance", root,
		"--work-root", workRoot,
		"--blob-store", blobRoot,
		"--daemon-api", daemonAPI,
		"--dispatch-namespace", "goobers-stages",
		"--task-queue", "goobers-engine",
		"--temporal-hostport", "temporal.svc:7233",
		"--temporal-namespace", "goobers",
	)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !host.ran {
		t.Fatal("the constructed worker host was never served")
	}
	if !strings.Contains(stdout, "drained cleanly") {
		t.Fatalf("stdout does not report the clean drain:\n%s", stdout)
	}

	if built.HostPort != "temporal.svc:7233" || built.Namespace != "goobers" {
		t.Errorf("host dials %s (namespace %s), want temporal.svc:7233 (namespace goobers)", built.HostPort, built.Namespace)
	}

	// The served queue set: the operator's workflow queue PLUS every
	// per-(gaggle x runner) dispatch queue the inventory implies. A dispatch
	// queue missing here is a stage nobody polls for.
	wantDispatchQueue := dispatcher.QueueName("example", "linux-pod")
	if len(built.TaskQueues) == 0 || built.TaskQueues[0] != "goobers-engine" {
		t.Errorf("task queues = %v, want the operator's queue first", built.TaskQueues)
	}
	if !slices.Contains(built.TaskQueues, wantDispatchQueue) {
		t.Errorf("task queues = %v, missing derived dispatch queue %s", built.TaskQueues, wantDispatchQueue)
	}

	// The mode-3 dispatch seam: pods land in the namespace the operator named,
	// and their surrender rides the --blob-store volume.
	if built.Deps.Dispatcher == nil || built.Deps.Surrenders == nil {
		t.Error("mode-3 dispatch is not wired into the engine deps")
	}
	if dispatch.Namespace != "goobers-stages" {
		t.Errorf("dispatcher namespace = %q, want goobers-stages", dispatch.Namespace)
	}
	if dispatch.WriteAPIBase != daemonAPI {
		t.Errorf("dispatcher write API base = %q, want %q", dispatch.WriteAPIBase, daemonAPI)
	}
	if surrender := filepath.Join(blobRoot, "surrender"); !dirExists(surrender) {
		t.Errorf("surrender plane %s was not provisioned under --blob-store", surrender)
	}
	if !strings.Contains(stdout, blobRoot) {
		t.Errorf("stdout does not report the artifact store at %s:\n%s", blobRoot, stdout)
	}

	// Live journal: the daemon write API the operator named is what the
	// emitter posts to.
	emitter, ok := built.Deps.Journal.(*livejournal.HTTPEmitter)
	if !ok {
		t.Fatalf("journal emitter = %T, want *livejournal.HTTPEmitter", built.Deps.Journal)
	}
	if emitter.BaseURL != daemonAPI {
		t.Errorf("live journal base URL = %q, want %q", emitter.BaseURL, daemonAPI)
	}

	// Work root: the credentialed provisioner that replaced the boot-time one
	// still scratches under the operator's --work-root.
	workspaces, ok := built.Deps.Workspaces.(*workerWorkspaces)
	if !ok {
		t.Fatalf("workspaces = %T, want the instance-credentialed *workerWorkspaces", built.Deps.Workspaces)
	}
	if want := filepath.Join(workRoot, "scratch"); workspaces.scratchRoot != want {
		t.Errorf("scratch root = %q, want %q", workspaces.scratchRoot, want)
	}
	if !dirExists(workerWorkcopiesDir(workRoot)) {
		t.Errorf("work root %s was never claimed and laid out", workRoot)
	}

	// The instance seams: without these the agentic and deterministic stages
	// fail closed, and the dispatch canary has no registry to refuse leaked
	// credentials against.
	if built.Deps.Goober == nil || built.Deps.Det == nil || built.Deps.Canary == nil {
		t.Error("instance-backed executor seams are not wired despite --instance")
	}
	if !strings.Contains(stdout, "config reload every") {
		t.Errorf("stdout does not report the config-reload wiring:\n%s", stdout)
	}
}

// The drain contract the rollout alerts on: work cut short is exit 3, not a
// clean 0. Asserted through the same seam, in the instance-less (mode-1/2)
// worker shape.
func TestRunWorkerReportsAbandonedDrainFromTheWorkerHost(t *testing.T) {
	withNeutralWorkerEnv(t)
	built, _ := withFakeWorkerHost(t, workerhost.ErrAbandonedWork)

	code, _, stderr := runArgs(t, "worker",
		"--work-root", filepath.Join(t.TempDir(), "work"),
		"--temporal-hostport", "temporal.svc:7233",
		"--temporal-namespace", "goobers",
		"--drain-timeout", "5s",
	)
	if code != workerAbandonedExit {
		t.Fatalf("exit = %d, want %d (abandoned drain)\nstderr: %s", code, workerAbandonedExit, stderr)
	}
	if built.DrainTimeout != 5*time.Second {
		t.Errorf("drain timeout = %s, want 5s", built.DrainTimeout)
	}
	if len(built.TaskQueues) != 1 || built.TaskQueues[0] != instance.DefaultEngineTaskQueue {
		t.Errorf("task queues = %v, want the configured default queue only", built.TaskQueues)
	}
	// Without --instance the worker keeps its self-only shape: workspaces and
	// automated gates, every executor stage failing closed by name.
	if built.Deps.Goober != nil || built.Deps.Det != nil || built.Deps.Dispatcher != nil {
		t.Error("executor/dispatch seams wired without --instance")
	}
}

// queueStubClient answers only DescribeTaskQueue, and records the dial options
// engine-queues resolved.
type queueStubClient struct {
	client.Client
	describer *fakeQueueDescriber
	closed    bool
}

func (c *queueStubClient) DescribeTaskQueue(ctx context.Context, queue string, kind enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error) {
	return c.describer.DescribeTaskQueue(ctx, queue, kind)
}

func (c *queueStubClient) Close() { c.closed = true }

// engine-queues is the queue-ownership evidence surface, so its own dispatch
// path has to prove two things the helper-level tests cannot: that the
// frontend it describes against is the one the instance config and flags
// resolve to, and that the queue SET it asks about is the derived one — the
// same set `goobers worker --dispatch-namespace` serves. A command that
// described only the workflow queue would report a clean answer while every
// dispatch queue went unowned.
func TestRunEngineQueuesDescribesTheDerivedQueueSetOnTheResolvedFrontend(t *testing.T) {
	root := cloudInstance(t)
	dispatchQueue := dispatcher.QueueName("example", "linux-pod")
	describer := &fakeQueueDescriber{pollers: map[string][]string{
		"goobers-engine/workflow":   {"goobers-worker/v1@goobers-worker-0#1"},
		"goobers-engine/activity":   {"goobers-worker/v1@goobers-worker-0#1"},
		dispatchQueue + "/workflow": {"goobers-worker/v1@goobers-worker-0#1"},
		dispatchQueue + "/activity": {"goobers-worker/v1@goobers-worker-0#1"},
	}}
	stub := &queueStubClient{describer: describer}
	var dialed client.Options
	previous := dialEngineTemporal
	dialEngineTemporal = func(_ context.Context, opts client.Options) (client.Client, error) {
		dialed = opts
		return stub, nil
	}
	t.Cleanup(func() { dialEngineTemporal = previous })

	code, stdout, stderr := runArgs(t, "engine-queues", "--json", root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}
	if dialed.HostPort != "temporal.svc:7233" || dialed.Namespace != "goobers" {
		t.Errorf("dialed %s (namespace %s), want the instance's engine config", dialed.HostPort, dialed.Namespace)
	}
	if !stub.closed {
		t.Error("engine-queues leaked its Temporal client")
	}

	var rows []queuePollers
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout)
	}
	described := map[string]queuePollers{}
	for _, row := range rows {
		described[row.Queue+"/"+row.Type] = row
	}
	for _, key := range []string{
		"goobers-engine/workflow", "goobers-engine/activity",
		dispatchQueue + "/workflow", dispatchQueue + "/activity",
	} {
		row, ok := described[key]
		if !ok {
			t.Fatalf("%s was never described; report covers %v", key, described)
		}
		if len(row.Pollers) != 1 {
			t.Errorf("%s pollers = %v, want the worker identity the frontend reported", key, row.Pollers)
		}
	}
	if !described[dispatchQueue+"/activity"].Dispatch {
		t.Errorf("%s is not marked as a dispatch queue, so a checker cannot select the dispatch set", dispatchQueue)
	}
}

// A describe failure is exit 1, not a silently empty ownership report.
func TestRunEngineQueuesFailsWhenTheFrontendRefuses(t *testing.T) {
	root := cloudInstance(t)
	previous := dialEngineTemporal
	dialEngineTemporal = func(context.Context, client.Options) (client.Client, error) {
		return nil, errors.New("connection refused")
	}
	t.Cleanup(func() { dialEngineTemporal = previous })

	code, _, stderr := runArgs(t, "engine-queues", root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (dial failure)\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "dial temporal at temporal.svc:7233") {
		t.Errorf("stderr does not name the frontend it failed to reach:\n%s", stderr)
	}
}

// projectStubClient stands in for the Temporal client engine-project dials.
// It implements nothing: this test's run is already projected, so a query
// through it would be a REGRESSION — the command would be re-projecting a
// journal it already has.
type projectStubClient struct {
	client.Client
	closed bool
}

func (c *projectStubClient) Close() { c.closed = true }

// engine-project resolves a gaggle's runs directory, the read-model intake,
// and a frontend, and only then projects. The gaggle-scoped runs dir is the
// sharp one: written to the wrong directory a recovered journal is invisible
// to the instance that asked for it, and nothing fails.
func TestRunEngineProjectResolvesGaggleRunsDirAndFrontend(t *testing.T) {
	root := cloudInstance(t)
	runsDir := instance.NewLayout(root).ForGaggle("example").RunsDir()
	seedProjectedRun(t, runsDir, "run-4223")

	stub := &projectStubClient{}
	var dialed client.Options
	previous := dialEngineTemporal
	dialEngineTemporal = func(_ context.Context, opts client.Options) (client.Client, error) {
		dialed = opts
		return stub, nil
	}
	t.Cleanup(func() { dialEngineTemporal = previous })

	code, stdout, stderr := runArgs(t, "engine-project", "--gaggle", "example", "run-4223", root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}
	if dialed.HostPort != "temporal.svc:7233" || dialed.Namespace != "goobers" {
		t.Errorf("dialed %s (namespace %s), want the instance's engine config", dialed.HostPort, dialed.Namespace)
	}
	if !stub.closed {
		t.Error("engine-project leaked its Temporal client")
	}
	if want := filepath.Join(runsDir, "run-4223"); !strings.Contains(stdout, want) {
		t.Errorf("stdout %q does not name the gaggle-scoped journal directory %s", stdout, want)
	}
}

// The other half of the same wiring: the frontend it cannot reach is named,
// and the failure is exit 1 rather than a silent no-op.
func TestRunEngineProjectFailsWhenTheFrontendRefuses(t *testing.T) {
	root := cloudInstance(t)
	previous := dialEngineTemporal
	dialEngineTemporal = func(context.Context, client.Options) (client.Client, error) {
		return nil, errors.New("connection refused")
	}
	t.Cleanup(func() { dialEngineTemporal = previous })

	code, _, stderr := runArgs(t, "engine-project", "--gaggle", "example",
		"--temporal-hostport", "temporal.other:7233", "run-4224", root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (dial failure)\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "dial temporal at temporal.other:7233") {
		t.Errorf("stderr does not name the overridden frontend:\n%s", stderr)
	}
}

// seedProjectedRun writes a complete terminal journal for runID, which is what
// makes engine-project's already-projected path the one under test.
func seedProjectedRun(t *testing.T, runsDir, runID string) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "example",
		StartedAt:       time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := run.Append(journal.Event{
		Type:   journal.EventRunFinished,
		Status: string(journal.PhaseCompleted),
	}); err != nil {
		_ = run.Close()
		t.Fatalf("append run.finished: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
