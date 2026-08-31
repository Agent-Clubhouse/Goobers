package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnercap"
)

// The dispatch queues bind BESIDE the operator's own queues: order preserved,
// duplicates not re-added, so a queue named both ways is served by exactly
// one worker in this process.
func TestMergeQueues(t *testing.T) {
	got := mergeQueues(
		[]string{"goobers-engine", "goobers-dispatch.web.win-ci"},
		[]string{"goobers-dispatch.web.win-ci", "goobers-dispatch.web.linux-xl"},
	)
	want := []string{"goobers-engine", "goobers-dispatch.web.win-ci", "goobers-dispatch.web.linux-xl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeQueues = %v, want %v", got, want)
	}
}

// The dispatch wiring fails closed before any cluster contact: no surrender
// plane, or no loadable instance, refuses with the cause named.
func TestBuildStageDispatchFailsClosed(t *testing.T) {
	t.Run("missing instance", func(t *testing.T) {
		_, err := buildStageDispatch(t.TempDir(), "gaggle-web", "", t.TempDir(), "goobers-worker-0", nil)
		if err == nil || !strings.Contains(err.Error(), "instance config") {
			t.Fatalf("error = %v, want the instance-load refusal", err)
		}
	})
	t.Run("missing surrender plane", func(t *testing.T) {
		_, err := buildStageDispatch(t.TempDir(), "gaggle-web", "", "", "goobers-worker-0", nil)
		if err == nil || !strings.Contains(err.Error(), "surrender plane") {
			t.Fatalf("error = %v, want the surrender-plane requirement named", err)
		}
	})
}

// The one WIRING the env:default-deny work added, tested at the wiring rather
// than at the helper: the instance's operator-declared `runner.envPassthrough`
// must reach dispatcher.Config, or the operator's escape hatch exists on the
// local substrate (shell.ExtraEnvAllowlist) and is dead on the pod substrate.
//
// podspec_test sets Config.EnvPassthrough directly, so it proves the ALLOWLIST
// honours the field and proves nothing about who fills it. Deleting the one
// line that fills it leaves every other test in the tree green, and the failure
// surfaces as a var an operator declared going missing on a restricted runner
// class only — the same restriction-conditional, far-side diagnosis #3725 is
// about (#3725/#736).
//
// The assertion runs the whole way through: instance config -> built Config ->
// the allowlist actually stamped on a rendered pod.
func TestBuildStageDispatchThreadsInstanceEnvPassthroughToTheStagePod(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Runner.EnvPassthrough = []string{"OPERATOR_DECLARED_VAR"}
	// A non-self runner dispatches through the engine connection, so the
	// inventory below is only declarable alongside an engine: block.
	cfg.Engine = &instance.EngineConfig{
		HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "goobers-engine",
	}
	cfg.Runners = append(cfg.Runners, instance.RunnerEntry{
		Name: "linux-shell-envdeny",
		Host: "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
		Provides: instance.RunnerProvides{
			OS: "linux", CPU: "2000m", Memory: "4Gi", Disk: "20Gi", Shell: true,
		},
		Restrictions: []instance.RunnerRestriction{instance.RunnerRestriction(runnercap.RestrictionEnvDefaultDeny)},
	})
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	previousClient := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return fake.NewClientset(), nil }
	t.Cleanup(func() { dispatchKubeClient = previousClient })

	var built dispatcher.Config
	previousNew := newStageDispatcher
	newStageDispatcher = func(c dispatcher.Config, pods dispatcher.PodAPI, journal dispatcher.JournalRelay, gate dispatcher.SurrenderGate, capacity dispatcher.CapacityProber) (*dispatcher.Dispatcher, error) {
		built = c
		return previousNew(c, pods, journal, gate, capacity)
	}
	t.Cleanup(func() { newStageDispatcher = previousNew })

	if _, err := buildStageDispatch(root, "gaggle-example", "", t.TempDir(), "goobers-worker-0", workerReloadSeams(t, root)); err != nil {
		t.Fatalf("buildStageDispatch: %v", err)
	}
	if !slices.Contains(built.EnvPassthrough, "OPERATOR_DECLARED_VAR") {
		t.Fatalf("dispatcher.Config.EnvPassthrough = %v, want the instance's runner.envPassthrough — "+
			"without it the operator's env:default-deny hatch is dead on the pod substrate (#3725/#736)", built.EnvPassthrough)
	}

	// Far side of the same wiring: the Config this worker built must actually
	// put that name on a stage pod's allowlist.
	pod, err := dispatcher.RenderPod(built, dispatcher.Attempt{
		RunID: "run-3725", Gaggle: "example", Workflow: "implementation", Stage: "probe", Number: 1,
	}, dispatcher.RunnerSpec{
		Name:         "linux-shell-envdeny",
		OS:           "linux",
		HostKind:     instance.RunnerHostImage,
		Host:         "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
		Restrictions: []string{string(runnercap.RestrictionEnvDefaultDeny)},
	})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	var allow []string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == dispatcher.EnvStageEnvAllow {
			if err := json.Unmarshal([]byte(e.Value), &allow); err != nil {
				t.Fatalf("decode %s: %v", dispatcher.EnvStageEnvAllow, err)
			}
		}
	}
	if !slices.Contains(allow, "OPERATOR_DECLARED_VAR") {
		t.Fatalf("%s = %v, missing the operator's declared passthrough", dispatcher.EnvStageEnvAllow, allow)
	}
}

// The #3914 wiring, in exactly the same shape and for exactly the same
// reason: the bot login can only be resolved where the instance config is
// readable — HERE, in the worker — and podspec_test sets Config.BotLogins
// directly, so it proves the STAMP honours the field and proves nothing about
// who fills it. Deleting the one line that fills it leaves every other test in
// the tree green and the far side dead: stages in a pod silently lose their
// declared identity and regress to GET /user, which a GitHub App installation
// token cannot call.
//
// Asserted the whole way through: instance config -> built Config -> the
// value actually stamped on a rendered goobers-CLI stage pod.
func TestBuildStageDispatchThreadsTheConfiguredBotLoginToTheStagePod(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Engine = &instance.EngineConfig{
		HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "goobers-engine",
	}
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

	previousClient := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return fake.NewClientset(), nil }
	t.Cleanup(func() { dispatchKubeClient = previousClient })

	var built dispatcher.Config
	previousNew := newStageDispatcher
	newStageDispatcher = func(c dispatcher.Config, pods dispatcher.PodAPI, journal dispatcher.JournalRelay, gate dispatcher.SurrenderGate, capacity dispatcher.CapacityProber) (*dispatcher.Dispatcher, error) {
		built = c
		return previousNew(c, pods, journal, gate, capacity)
	}
	t.Cleanup(func() { newStageDispatcher = previousNew })

	if _, err := buildStageDispatch(root, "gaggle-example", "", t.TempDir(), "goobers-worker-0", workerReloadSeams(t, root)); err != nil {
		t.Fatalf("buildStageDispatch: %v", err)
	}
	if got := built.BotLogins[instance.GitHubBotLoginKey(repo.Owner, repo.Name)]; got != "goobersbot[bot]" {
		t.Fatalf("dispatcher.Config.BotLogins[%s/%s] = %q, want %q — without it no stage pod can resolve its own identity (#3914)",
			repo.Owner, repo.Name, got, "goobersbot[bot]")
	}

	pod, renderErr := dispatcher.RenderPod(built, dispatcher.Attempt{
		RunID: "run-3914", Gaggle: "example", Workflow: "implementation", Stage: "apply-verdict", Number: 1,
		CLIStage: true,
		RunContext: map[string]string{
			"GOOBERS_REPO_PROVIDER": "github",
			"GOOBERS_REPO_OWNER":    repo.Owner,
			"GOOBERS_REPO_NAME":     repo.Name,
		},
	}, dispatcher.RunnerSpec{
		Name:     "linux-cli",
		OS:       "linux",
		HostKind: instance.RunnerHostImage,
		Host:     "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
	})
	if renderErr != nil {
		t.Fatalf("RenderPod: %v", renderErr)
	}
	var stamped string
	var present bool
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == dispatcher.ProviderBotLoginEnv {
			stamped, present = e.Value, true
		}
	}
	if !present || stamped != "goobersbot[bot]" {
		t.Fatalf("%s on the rendered pod = %q (present=%v), want %q", dispatcher.ProviderBotLoginEnv, stamped, present, "goobersbot[bot]")
	}
}
