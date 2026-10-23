package childengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/childworkflow"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/workflow"
)

const childEnabledParent = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: default-implement
  annotations: {goobers.dev/allow-preview-features: "true"}
spec:
  gaggle: example
  triggers: [{type: manual}]
  start: plan
  tasks:
    - name: plan
      type: agentic
      goober: coder
      goal: Plan a child workflow
      capabilities: [agent:model]
      childWorkflows:
        allowedGoobers: [coder]
        allowedCapabilities: [agent:model, repo:push]
        allowPRPublication: true
`

// treeSource serves one retained generation from a directory.
type treeSource struct {
	generation, directory string
	set                   *instance.ConfigSet
}

func (s treeSource) Acquire(_ context.Context, generation string) (childworkflow.Generation, error) {
	if generation != s.generation {
		return childworkflow.Generation{}, os.ErrNotExist
	}
	return childworkflow.Generation{Directory: s.directory, Set: s.set, Release: func() error { return nil }}, nil
}

func loadSet(t *testing.T, directory string) *instance.ConfigSet {
	t.Helper()
	set, report, err := instance.LoadConfigDir(directory)
	if err != nil {
		t.Fatalf("load %s: %v (%s)", directory, err, runtimewiring.ValidationIssueSummary(report))
	}
	return set
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// parentPins compiles the parent stage from a config tree the way acceptance
// pinned it: its workflow digest and Goober digest.
func parentPins(t *testing.T, cfg *instance.Config, directory string, set *instance.ConfigSet) (workflowDigest, gooberDigest, goal string) {
	t.Helper()
	selected := childworkflow.ParentSelection{Gaggle: "example", Workflow: "default-implement", Stage: "plan"}
	pinned, machine, err := childworkflow.StageCatalog(cfg, set, selected, childworkflow.BackendEngine, childworkflow.OfflineGooberAdmission)
	if err != nil {
		t.Fatal(err)
	}
	instructions, err := runtimewiring.LoadGooberInstructions(directory, set, pinned.Goobers)
	if err != nil {
		t.Fatal(err)
	}
	skills, err := runtimewiring.LoadGooberSkillPackages(directory, "example", pinned.Goobers)
	if err != nil {
		t.Fatal(err)
	}
	goobers, err := workflow.ComputeGooberDigest(machine.Def, pinned.Goobers, instructions, skills)
	if err != nil {
		t.Fatal(err)
	}
	return machine.Digest(), goobers, pinned.ParentTask.Goal
}

// The resolver reproduces the parent's pins from the RETAINED generation the
// envelope names, while the applied (live) configuration has since edited the
// parent workflow. A source that served live config instead is refused.
func TestPinnedResolverLoadsPinsFromTheRetainedGenerationNotLiveConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	layout := instance.NewLayout(root)
	parentFile := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(parentFile, []byte(childEnabledParent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	retainedDir := filepath.Join(t.TempDir(), "retained-generation")
	copyTree(t, layout.ConfigDir(), retainedDir)
	retained := loadSet(t, retainedDir)
	retainedWorkflow, retainedGoober, retainedGoal := parentPins(t, cfg, retainedDir, retained)

	// Live configuration moves on after acceptance.
	edited := strings.Replace(childEnabledParent, "goal: Plan a child workflow", "goal: Plan a child workflow, edited live", 1)
	if err := os.WriteFile(parentFile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	live := loadSet(t, layout.ConfigDir())
	liveWorkflow, _, _ := parentPins(t, cfg, layout.ConfigDir(), live)
	if liveWorkflow == retainedWorkflow {
		t.Fatal("fixture did not change the live parent workflow")
	}

	e := childworkflow.ChildStartEnvelope{Gaggle: "example", ParentRunID: strings.Repeat("a", 32), ParentStage: "plan", ParentWorkflow: "default-implement",
		ConfigGeneration: "sha256:retained-generation", ParentWorkflowDigest: retainedWorkflow, ParentGooberDigest: retainedGoober}
	applied := appliedConfig{candidate: cloudconfig.Candidate{Digest: "sha256:live-generation", Config: cfg, Set: live}, ok: true}
	resolve, err := PinnedResolver("instance-a", treeSource{generation: e.ConfigGeneration, directory: retainedDir, set: retained}, applied)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := resolve(t.Context(), e)
	if err != nil {
		t.Fatalf("resolve from the retained generation: %v", err)
	}
	switch {
	case pinned.ConfigGeneration != e.ConfigGeneration:
		t.Fatalf("generation = %q, want the envelope's %q, not the applied one", pinned.ConfigGeneration, e.ConfigGeneration)
	case pinned.ParentWorkflowDigest != retainedWorkflow || pinned.ParentWorkflowDigest == liveWorkflow:
		t.Fatalf("parent workflow digest %q is not the retained one", pinned.ParentWorkflowDigest)
	case pinned.ParentGooberDigest != retainedGoober:
		t.Fatalf("parent goober digest %q is not the retained one", pinned.ParentGooberDigest)
	case pinned.Admission.ParentTask.Goal != retainedGoal:
		t.Fatalf("pinned stage goal %q came from live config", pinned.Admission.ParentTask.Goal)
	case pinned.Execution.InstanceID != "instance-a":
		t.Fatalf("execution instance %q is not this control plane's", pinned.Execution.InstanceID)
	}

	// Serving the live tree under the pinned generation cannot pass.
	wrong, err := PinnedResolver("instance-a", treeSource{generation: e.ConfigGeneration, directory: layout.ConfigDir(), set: live}, applied)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong(t.Context(), e); err == nil {
		t.Fatal("resolved a retained generation's pins from live config")
	}
}
