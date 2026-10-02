package runtimeplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestIdentityNeverAttestsInteractiveChecksAsTarget(t *testing.T) {
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, kind := range []string{"", "self", "image", "deployment", "unknown"} {
			for _, account := range []string{"operator", "service-account"} {
				t.Run(platform+"/"+kind+"/"+account, func(t *testing.T) {
					process := Process{PID: 42, OS: platform, UID: account, Source: Source{Fidelity: "observed"}}
					got := TargetIdentity(process, kind)
					if got.Outcome != "unsupported" || got.Source.Fidelity != "unobservable" || got.Code == "" {
						t.Fatalf("unverified target passed: %+v", got)
					}
					if (kind == "image" || kind == "deployment") && got.Code != "worker_identity_unobservable" {
						t.Fatalf("worker limitation: %+v", got)
					}
				})
			}
		}
	}
	if got := TargetIdentity(Process{OS: "windows"}, "self"); got.Code != "process_identity_unobservable" {
		t.Fatalf("missing token accepted: %+v", got)
	}
}

func TestObserveProcessIgnoresIdentityEnvironment(t *testing.T) {
	t.Setenv("USER", "not-the-os-user")
	t.Setenv("USERNAME", "not-the-os-user")
	got := ObserveProcess()
	if got.PID != os.Getpid() || got.OS == "" || got.UID == "not-the-os-user" {
		t.Fatalf("process = %+v", got)
	}
	if got.UID == "" && got.Source.Fidelity != "unobservable" {
		t.Fatalf("missing identity presented as observed: %+v", got)
	}
}

func TestResolveInputsPreservesIsolationAndPathsWithoutReadingCredentials(t *testing.T) {
	root := t.TempDir()
	workcopies := filepath.Join(root, "alternate-workcopies")
	cfg := &instance.Config{
		Workcopies:  &instance.WorkcopiesConfig{Root: workcopies},
		Sandbox:     &instance.SandboxConfig{Agentic: "enforced"},
		Repos:       []instance.RepoRef{{Token: instance.TokenRef{File: "/missing/secret-file"}, DefaultStageTimeout: "3h"}},
		Credentials: []instance.CredentialGrant{{Capability: "agent:model", Harness: "copilot", Token: instance.TokenRef{Env: "SECRET_VARIABLE"}}},
		Runners:     []instance.RunnerEntry{{Name: "worker", Host: "example/worker:latest", Restrictions: []instance.RunnerRestriction{"no-host-mounts"}}},
		Runner:      instance.RunnerConfig{DefaultStageTimeout: "2h", RequiredMCPSettleTimeout: "40s"},
	}
	got := ResolveInputs(instance.Layout{Root: root}.ForGaggle("sample"), cfg, apiv1.GaggleSpec{})
	if got.Sandbox.Agentic != "enforced" || got.Sandbox.Enforcement.Outcome != "unobservable" {
		t.Fatalf("sandbox = %+v", got.Sandbox)
	}
	if len(got.Sandbox.Runners) != 1 || len(got.Sandbox.Runners[0].Restrictions) != 1 {
		t.Fatalf("runner restrictions missing: %+v", got.Sandbox)
	}
	if got.Timeouts.RepositoryDefaults[0] != "3h" || got.Timeouts.RunnerDefault != "2h" {
		t.Fatalf("timeouts = %+v", got.Timeouts)
	}
	found := false
	for _, path := range got.Paths {
		if path.Purpose == "workcopies" && path.Path == filepath.Join(workcopies, "sample") {
			found = true
		}
	}
	if !found {
		t.Fatalf("execution workcopy path not preserved: %+v", got.Paths)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET_VARIABLE", "/missing/secret-file"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("credential ref leaked: %s", raw)
		}
	}
	if got.CredentialSources[0].Kind != "file" || got.CredentialSources[1].Kind != "env" {
		t.Fatalf("source kinds = %+v", got.CredentialSources)
	}
}

func TestStageTimeoutInputsRetainPrecedenceSurfaces(t *testing.T) {
	task := apiv1.Task{Name: "work", Goober: "coder", Workspace: "repo", TimeoutSeconds: 90, Limits: &apiv1.Limits{MaxDurationSeconds: 45}, Inputs: map[string]string{"timeout": "30s"}, OnTimeout: "salvage"}
	settings := ResolveStages(apiv1.WorkflowSpec{Tasks: []apiv1.Task{task}}, map[string]apiv1.GooberSpec{"coder": {TimeoutSeconds: 120}})["work"]
	if settings.TimeoutSeconds != 90 || settings.GooberTimeoutSeconds != 120 || settings.Limits.MaxDurationSeconds != 45 || settings.LegacyTimeout != "30s" || settings.Workspace != "repo" || settings.OnTimeout != "salvage" {
		t.Fatalf("lost execution inputs: %+v", settings)
	}
}
