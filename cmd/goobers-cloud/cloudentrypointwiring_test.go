package main

import (
	"bytes"
	"context"
	"fmt"
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
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/temporaldial"
	"github.com/goobers/goobers/internal/workerhost"
)

// cloudentrypointwiring_test.go covers the legacy `goobers-cloud worker` entry point
// until its separate removal.
//
// Same shape as workerwiring_routing_test.go: real production wiring, only
// the transport (Temporal, the cluster) substituted at the existing seams.

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
		"--health-addr", "127.0.0.1:8081",
		"--max-concurrent-activities", "7",
		"--config-reload-interval", "0",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr.String())
	}

	if want := []string{"goobers-engine", "goobers-engine-windows"}; !slices.Equal(got.TaskQueues, want) {
		t.Errorf("task queues = %v, want %v", got.TaskQueues, want)
	}
	if !strings.HasPrefix(stderr.String(), manualServiceRootHeader(t, root)) {
		t.Fatalf("successful worker startup omitted its root identity: %s", stderr.String())
	}
	if got.HostPort != "temporal.goobers.svc:7233" || got.Namespace != "goobers-cloud" {
		t.Errorf("frontend = %s (namespace %s), want the flag values", got.HostPort, got.Namespace)
	}
	if got.DrainTimeout.String() != "7s" {
		t.Errorf("drain timeout = %s, want 7s", got.DrainTimeout)
	}
	if got.HealthAddr != "127.0.0.1:8081" || got.MaxConcurrentActivities != 7 {
		t.Errorf("health/concurrency = %q/%d, want 127.0.0.1:8081/7", got.HealthAddr, got.MaxConcurrentActivities)
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
	if want := runtimewiring.WorkerScratchDir(workRoot); workspaces.scratchRoot != want {
		t.Errorf("scratch root = %q, want %q", workspaces.scratchRoot, want)
	}
	registry, ok := got.Deps.Canary.(*journal.RegistryScrubber)
	if !ok {
		t.Fatalf("canary = %T, want the worker's live registry", got.Deps.Canary)
	}
	const secret = "SUPER-SECRET-CANARY-9f8e7d6c5b4a3210"
	registry.Register([]byte(secret))
	if scrubbed := got.Deps.Scrubber.Scrub([]byte(secret)); bytes.Contains(scrubbed, []byte(secret)) {
		t.Fatalf("worker host scrubber does not share its live registry: %q", scrubbed)
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
	configureDispatchAuthority(t, root)
	blobRoot := filepath.Join(t.TempDir(), "blobs")
	server, _ := workerBlobDaemon(t, root, blobRoot)
	t.Setenv("GOOBERS_BLOB_ENDPOINT", server.URL)
	got := captureWorkerHost(t)

	previousKube := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return fake.NewClientset(), nil }
	t.Cleanup(func() { dispatchKubeClient = previousKube })
	previousPreflight := preflightGaggleNamespaces
	preflightGaggleNamespaces = fakeClusterHasNoRealRBACSoSkipPreflight
	t.Cleanup(func() { preflightGaggleNamespaces = previousPreflight })
	var built dispatcher.Config
	previousNew := newStageDispatcher
	newStageDispatcher = func(c dispatcher.Config, pods dispatcher.PodAPI, journal dispatcher.JournalRelay, gate dispatcher.SurrenderGate, capacity dispatcher.CapacityProber) (*dispatcher.Dispatcher, error) {
		built = c
		return previousNew(c, pods, journal, gate, capacity)
	}
	t.Cleanup(func() { newStageDispatcher = previousNew })
	// The boot-time orphan sweep is the worker's only Temporal contact before
	// it polls; refusing the dial exercises its skip path without a frontend.
	previousDial := dialWorkerSweepTemporal
	dialWorkerSweepTemporal = func(string, string, *temporaldial.TLS, ...converter.DataConverter) (client.Client, error) {
		return nil, context.DeadlineExceeded
	}
	t.Cleanup(func() { dialWorkerSweepTemporal = previousDial })

	var stdout, stderr bytes.Buffer
	code := runWorker([]string{
		"--instance", root,
		"--work-root", filepath.Join(t.TempDir(), "work"),
		"--blob-store", blobRoot,
		"--dispatch-namespace", "goobers-stages",
		"--daemon-api", "https://daemon.example:8080",
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
	// #4897: the flag's own value no longer selects a pod namespace, so the
	// startup line describes per-gaggle routing instead, and the Config
	// actually wired maps the "example" gaggle to ITS OWN declared
	// isolation.namespace (the starter scaffold's gaggle-example) — never the
	// flag's "goobers-stages" value.
	if !strings.Contains(stdout.String(), "isolation.namespace") {
		t.Errorf("startup output does not describe per-gaggle isolation.namespace routing:\n%s", stdout.String())
	}
	if got := built.GaggleNamespaces["example"]; got != "gaggle-example" {
		t.Errorf("dispatcher.Config.GaggleNamespaces[example] = %q, want the gaggle's own declared isolation.namespace %q", got, "gaggle-example")
	}
}

// --dispatch-namespace without --instance has no runner inventory to derive
// queues from, so it must refuse rather than start a worker that polls
// nothing it was asked to.
// #5950: engine.workerVersioning is the only switch that makes the worker poll
// versioned; without it the worker host is built unversioned.
func TestRunWorkerWorkerVersioningFollowsEngineConfig(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(fmt.Sprintf("workerVersioning=%v", optIn), func(t *testing.T) {
			root := initDemo(t)
			layout := instance.NewLayout(root)
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			cfg.Engine = &instance.EngineConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "goobers-engine", WorkerVersioning: optIn}
			if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			got := captureWorkerHost(t)
			var stdout, stderr bytes.Buffer
			code := runWorker([]string{"--instance", root, "--work-root", filepath.Join(t.TempDir(), "work"), "--blob-store", t.TempDir(), "--config-reload-interval", "0"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr.String())
			}
			if got.Versioning != optIn {
				t.Fatalf("workerhost.Config.Versioning = %v, want %v", got.Versioning, optIn)
			}
			if want := fmt.Sprintf("worker versioning %s", onOff(optIn)); !strings.Contains(stdout.String(), want) {
				t.Fatalf("startup output does not say %q:\n%s", want, stdout.String())
			}
		})
	}
}

func TestRunWorkerDispatchNamespaceRequiresInstance(t *testing.T) {
	t.Setenv("GOOBERS_INSTANCE_ROOT", "")
	got := captureWorkerHost(t)
	previousKube := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) {
		t.Fatal("missing instance reached Kubernetes")
		return nil, nil
	}
	t.Cleanup(func() { dispatchKubeClient = previousKube })
	workRoot := filepath.Join(t.TempDir(), "work")
	var stdout, stderr bytes.Buffer
	code := runWorker([]string{
		"--work-root", workRoot,
		"--dispatch-namespace", "goobers-stages",
		"--daemon-api", "https://user:do-not-print@daemon.example:8080",
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
	if _, err := os.Stat(workRoot); !os.IsNotExist(err) {
		t.Fatalf("missing instance initialized runtime: %v", err)
	}
	if strings.Contains(stdout.String()+stderr.String(), "do-not-print") {
		t.Fatal("missing-instance refusal exposed URL credentials")
	}
}

func TestRunWorkerRefusesUnusableDispatchAuthorityBeforeStarting(t *testing.T) {
	for _, tc := range []struct{ name, daemon, blob, key, want string }{
		{"missing daemon", "", "https://daemon.example:8080", "valid", "--daemon-api"},
		{"invalid daemon", "https://user:do-not-print@daemon.example", "https://daemon.example:8080", "valid", "--daemon-api"},
		{"missing blob", "https://daemon.example:8080", "", "valid", "GOOBERS_BLOB_ENDPOINT"},
		{"invalid blob", "https://daemon.example:8080", "https://daemon.example?do-not-print", "valid", "GOOBERS_BLOB_ENDPOINT"},
		{"missing key", "https://daemon.example:8080", "https://daemon.example:8080", "missing", "api.podTokenKeyFile"},
		{"unreadable key", "https://daemon.example:8080", "https://daemon.example:8080", "unreadable", "api.podTokenKeyFile"},
		{"short key", "https://daemon.example:8080", "https://daemon.example:8080", "short", "api.podTokenKeyFile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			declareDispatchRunner(t, root, "linux-pod")
			configureDispatchAuthority(t, root)
			t.Setenv("GOOBERS_DAEMON_API", "")
			t.Setenv("GOOBERS_BLOB_ENDPOINT", tc.blob)
			layout := instance.NewLayout(root)
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			switch tc.key {
			case "missing":
				cfg.API.PodTokenKeyFile = ""
			case "unreadable":
				cfg.API.PodTokenKeyFile = filepath.Join(t.TempDir(), "missing.key")
			case "short":
				if err := os.WriteFile(cfg.API.PodTokenKeyFile, []byte("do-not-print"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
				t.Fatal(err)
			}
			previous := dispatchKubeClient
			dispatchKubeClient = func() (kubernetes.Interface, error) {
				t.Fatal("invalid configuration reached Kubernetes")
				return nil, nil
			}
			t.Cleanup(func() { dispatchKubeClient = previous })
			got := captureWorkerHost(t)
			workRoot := filepath.Join(t.TempDir(), "work")
			var stdout, stderr bytes.Buffer
			code := runWorker([]string{"--instance", root, "--work-root", workRoot, "--blob-store", t.TempDir(), "--dispatch-namespace", "goobers-stages", "--daemon-api", tc.daemon}, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit=%d stderr=%s, want usage refusal naming %s", code, &stderr, tc.want)
			}
			if len(got.TaskQueues) != 0 {
				t.Fatal("invalid configuration constructed a polling worker")
			}
			if _, err := os.Stat(workRoot); !os.IsNotExist(err) {
				t.Fatalf("invalid configuration initialized runtime: %v", err)
			}
			if strings.Contains(stdout.String()+stderr.String(), "do-not-print") {
				t.Fatal("configuration error exposed secret material")
			}
		})
	}
}
