package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

type pinnedChildFixture struct {
	layout       instance.Layout
	cfg          *instance.Config
	applied      *instance.ConfigSet
	parent       journal.RunIdentity
	stage        apiv1.Task
	sourcePath   string
	retainedPath string
}

func newPinnedChildFixture(t *testing.T, editConfig ...func(string)) pinnedChildFixture {
	t.Helper()
	root, sourcePath, _ := childValidationFixture(t)
	for _, edit := range editConfig {
		edit(root)
	}
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := layout.EnsureIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		t.Fatalf("config load: %v; findings: %+v", err, report)
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
	goobers := goobersByName(set)
	instructions, err := loadGooberInstructions(layout.ConfigDir(), set, goobers)
	if err != nil {
		t.Fatal(err)
	}
	// Pin with the existing daemon compiler, not the new loader's implementation.
	machines, digests, _, _, err := compileSchedulerMachinesWithProgress(layout, cfg, set, goobers, instructions, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "default-implement"}
	machine := machines[identity]
	data, generation, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), owner)
	if err != nil {
		t.Fatal(err)
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := store.Keep(t.Context(), data, generation, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	parent := journal.RunIdentity{InstanceID: owner, RunID: strings.Repeat("a", 32), Workflow: identity.Workflow, WorkflowVersion: machine.Def.Version, WorkflowDigest: machine.Digest(), GooberDigest: digests[identity], ConfigGeneration: generation, Gaggle: identity.Gaggle, Trigger: journal.Trigger{Kind: journal.TriggerManual}}
	run, err := journal.Create(layout.ForGaggle(identity.Gaggle).RunsDir(), parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	task, ok := machine.Task("plan")
	if !ok {
		t.Fatal("missing fixture stage")
	}
	return pinnedChildFixture{layout: layout, cfg: cfg, applied: set, parent: parent, stage: *task.DeepCopy(), sourcePath: sourcePath, retainedPath: retained}
}
func (f pinnedChildFixture) load(t *testing.T) childworkflow.Authority {
	t.Helper()
	a, err := loadPinnedChildStage(t.Context(), f.layout, f.cfg, f.applied, f.parent, "plan")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPinnedChildLoaderUsesArchiveAndAppliedAuthority(t *testing.T) {
	f := newPinnedChildFixture(t)
	first := f.load(t)
	if first.ParentWorkflow != f.parent.Workflow || first.ConfigGeneration != f.parent.ConfigGeneration || first.ParentWorkflowDigest != f.parent.WorkflowDigest || first.ParentGooberDigest != f.parent.GooberDigest || !reflect.DeepEqual(first.Admission.ParentTask, f.stage) {
		t.Fatalf("parent pins changed: %+v", first)
	}
	// Invalid pending disk edits cannot become applied authority. The old
	// immutable snapshot and retained archive still admit exactly the same grant.
	if err := os.WriteFile(f.sourcePath, []byte("invalid: ["), 0644); err != nil {
		t.Fatal(err)
	}
	second := f.load(t)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("pending mutable config changed child authority")
	}
	validator, err := childworkflow.NewValidator(second.Admission)
	if err != nil {
		t.Fatal(err)
	}
	p, err := validator.Validate([]byte(childValidationProposal))
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDigest != first.Origin.ConfigDigest || p.PolicyDigest != first.Origin.PolicyDigest {
		t.Fatal("tool grant and queued proposal pins diverged")
	}
	// The returned copy cannot mutate either applied authority or retained source.
	second.Admission.ParentTask.ChildWorkflows.AllowedCapabilities = nil
	second.Admission.Config.Runner.HarnessCommand = map[string][]string{"copilot": {"not-a-real-harness"}}
	if !reflect.DeepEqual(first, f.load(t)) {
		t.Fatal("caller mutation changed trusted snapshot")
	}
}

func TestPinnedChildLoaderNarrowsCurrentGrantsWithoutEditingPinnedTask(t *testing.T) {
	f := newPinnedChildFixture(t)
	before := f.load(t)
	for i := range f.applied.Workflows {
		if f.applied.Workflows[i].Name == f.parent.Workflow {
			f.applied.Workflows[i].Spec.Tasks[0].ChildWorkflows.AllowedCapabilities = nil
			f.applied.Workflows[i].Spec.Tasks[0].ChildWorkflows.AllowPRPublication = false
		}
	}
	after := f.load(t)
	if len(after.Admission.GrantedCapabilities) != 0 || after.Admission.AllowPRPublication || !reflect.DeepEqual(after.Admission.ParentTask, f.stage) {
		t.Fatalf("current narrowing altered pinned semantics: %+v", after.Admission)
	}
	if before.Origin.ConfigDigest != after.Origin.ConfigDigest || before.Origin.PolicyDigest == after.Origin.PolicyDigest {
		t.Fatal("config pin should remain fixed while effective policy changes")
	}
}

func TestPinnedChildLoaderRefusesMissingTamperedOrForeignCustody(t *testing.T) {
	for _, mode := range []string{"generation", "workflow-pin", "goober-pin", "foreign-instance", "missing-current", "disabled-current", "removed-child-policy", "reduced-allowance", "tampered-archive"} {
		t.Run(mode, func(t *testing.T) {
			f := newPinnedChildFixture(t)
			switch mode {
			case "generation":
				f.parent.ConfigGeneration = journal.Digest([]byte("unretained"))
			case "workflow-pin":
				f.parent.WorkflowDigest = journal.Digest([]byte("other workflow"))
			case "goober-pin":
				f.parent.GooberDigest = journal.Digest([]byte("other goober"))
			case "foreign-instance":
				f.parent.InstanceID = strings.Repeat("b", 32)
			case "missing-current":
				f.applied.Workflows = nil
			case "disabled-current":
				off := false
				f.applied.Gaggles[0].Spec.Enabled = &off
			case "removed-child-policy":
				for i := range f.applied.Workflows {
					if f.applied.Workflows[i].Name == f.parent.Workflow {
						f.applied.Workflows[i].Spec.Tasks[0].ChildWorkflows = nil
					}
				}
			case "reduced-allowance":
				for i := range f.applied.Workflows {
					if f.applied.Workflows[i].Name == f.parent.Workflow {
						f.applied.Workflows[i].Spec.Tasks[0].ChildWorkflows.MaxChildren = 1
					}
				}
			case "tampered-archive":
				relative, err := filepath.Rel(f.layout.ConfigDir(), f.sourcePath)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(f.retainedPath, relative), []byte(childValidationParent+"#tampered"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadPinnedChildStage(t.Context(), f.layout, f.cfg, f.applied, f.parent, "plan"); err == nil {
				t.Fatal("unverified authority accepted")
			}
		})
	}
}

func TestChildAdmissionRetainsGooberDefinitionDirectory(t *testing.T) {
	f := newPinnedChildFixture(t, func(root string) {
		directory := filepath.Join(root, "config", "gaggles", "example", "goobers")
		if err := os.Rename(filepath.Join(directory, "coder"), filepath.Join(directory, "curated-coder")); err != nil {
			t.Fatal(err)
		}
	})
	authority := f.load(t)
	if authority.ParentGooberDigest != f.parent.GooberDigest {
		t.Fatal("child admission lost the retained Goober instruction identity")
	}
}
