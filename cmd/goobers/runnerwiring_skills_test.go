package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestBuildAgenticExecutorDeliversResolvedSkillSnapshot(t *testing.T) {
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := harness.NewRegistry()
	adapter := &harness.FakeAdapter{Act: func(_ context.Context, req harness.RunRequest) error {
		data, err := os.ReadFile(filepath.Join(req.Workspace, ".github/skills/review/SKILL.md"))
		if err != nil {
			return err
		}
		if string(data) != "shared captured bytes" {
			t.Errorf("skill bytes %q", data)
		}
		return harness.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	}}
	if err := registry.RegisterAs(string(apiv1.HarnessCopilot), adapter); err != nil {
		t.Fatal(err)
	}
	scrubber := journal.NewRegistryScrubber()
	executor, err := buildAgenticExecutor(agenticExecutorInput{
		GooberName: "coder", Goobers: map[string]apiv1.GooberSpec{"coder": {Skills: []string{"review"}}},
		Instructions: map[string]string{"coder": "role instructions"},
		SkillPackages: map[string][]workflow.SkillFile{
			"review":                                 {{Path: "SKILL.md", Content: "gaggle bytes"}},
			workflow.SharedSkillPackageKey("review"): {{Path: "SKILL.md", Content: "shared captured bytes"}},
		},
		AdapterRegistry: registry, Resolver: resolver, SharedRegistry: scrubber, RunsDir: t.TempDir(),
		ArtifactRecorder: runnerWiringHarnessRecorder{dir: t.TempDir()}, SecretRegistrar: scrubber,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Invoke(context.Background(), apiv1.InvocationEnvelope{RunID: "run", TaskID: "task", Goober: "coder", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func TestGaggleFingerprintTracksCapturedSkillBytes(t *testing.T) {
	packages := map[string][]workflow.SkillFile{"review": {{Path: "SKILL.md", Content: "original"}}}
	digest := func() string {
		t.Helper()
		value, err := gaggleConfigFingerprint(&instance.Config{}, &instance.ConfigSet{}, "team", nil, nil, packages)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := digest()
	packages["review"][0].Content = "changed"
	if after := digest(); before == after {
		t.Fatal("skill-only edit reused stale executor seams")
	}
}
