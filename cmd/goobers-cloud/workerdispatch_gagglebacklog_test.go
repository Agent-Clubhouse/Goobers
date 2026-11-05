package main

import (
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
)

// TestBuildStageDispatchThreadsGaggleBacklogsToTheStagePod is #5957's wiring,
// in the shape of the #3914 bot-login test beside it: a gaggle's backlog
// settings can only be resolved where the instance config is readable, HERE in
// the worker. Asserted the whole way through: instance config -> built Config
// -> the value stamped on a rendered goobers-CLI stage pod.
func TestBuildStageDispatchThreadsGaggleBacklogsToTheStagePod(t *testing.T) {
	root := initDemo(t)
	configureDispatchAuthority(t, root)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Engine = &instance.EngineConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "goobers-engine"}
	cfg.Runners = append(cfg.Runners, instance.RunnerEntry{
		Name: "linux-cli",
		Host: "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
		Provides: instance.RunnerProvides{
			OS: "linux", CPU: "2000m", Memory: "4Gi", Disk: "20Gi", Shell: true,
		},
	})
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil || report.HasErrors() {
		t.Fatalf("LoadConfigDir: %v report=%+v", err, report)
	}
	want, err := stageGaggleBacklogStamps(set.Gaggles)
	if err != nil {
		t.Fatal(err)
	}

	previousClient := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return fake.NewClientset(), nil }
	t.Cleanup(func() { dispatchKubeClient = previousClient })
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

	if _, err := buildStageDispatch(root, "https://daemon.example:8080", t.TempDir(), "goobers-worker-0", workerReloadSeams(t, root), ""); err != nil {
		t.Fatalf("buildStageDispatch: %v", err)
	}
	if len(set.Gaggles) == 0 {
		t.Fatal("the demo instance declares no gaggle")
	}
	gaggle := set.Gaggles[0].Name
	if got := built.GaggleBacklogs[gaggle]; got == "" || got != want[gaggle] {
		t.Fatalf("dispatcher.Config.GaggleBacklogs[%s] = %q, want %q — without it no stage pod can apply its gaggle's backlog settings (#5957)", gaggle, got, want[gaggle])
	}

	pod, err := dispatcher.RenderPod(built, dispatcher.Attempt{
		RunID: "run-5957", Gaggle: gaggle, Workflow: "implementation", Stage: "query-backlog", Number: 1, CLIStage: true,
	}, dispatcher.RunnerSpec{
		Name: "linux-cli", OS: "linux", HostKind: instance.RunnerHostImage,
		Host: "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == dispatcher.GaggleBacklogEnv {
			if e.Value != want[gaggle] {
				t.Fatalf("%s on the rendered pod = %q, want %q", dispatcher.GaggleBacklogEnv, e.Value, want[gaggle])
			}
			return
		}
	}
	t.Fatalf("%s was not stamped on the rendered pod", dispatcher.GaggleBacklogEnv)
}
