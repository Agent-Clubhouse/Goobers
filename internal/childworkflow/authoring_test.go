package childworkflow

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

func authoringFixture() (Authority, *agentickit.Kit) {
	input := testContext()
	input.ParentTask.Goober = "coder"
	input.Goobers["coder"] = apiv1.GooberSpec{Gaggle: "web", Harness: apiv1.HarnessClaudeCode, Capabilities: []string{"agent:model", "repo:push"}, Model: "private-model-marker", Instructions: "private-instructions-marker"}
	input.Goobers["foreign"] = apiv1.GooberSpec{Gaggle: "other", Instructions: "foreign-goober-marker"}
	input.Config.Runners = []instance.RunnerEntry{
		{Name: "isolated", Host: "registry.example.invalid/private-image-marker:v1", Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux, Shell: true, Harnesses: []string{"claude-code"}, Capabilities: []string{"child-work"}}},
		{Name: "self", Host: "self"},
	}
	input.Config.Runner.HarnessCommand = map[string][]string{"claude-code": {"private-command-marker"}}
	a := Authority{Admission: input, Origin: Origin{Gaggle: "web", RunID: "parent-run", ConfigDigest: input.ConfigDigest, PolicyDigest: "policy-digest", StageOccurrence: "occurrence", AttemptID: "attempt"}, ConfigGeneration: "generation"}
	kit := &agentickit.Kit{Envelope: apiv1.InvocationEnvelope{RunID: a.Origin.RunID, Gaggle: "web", Goober: "coder", ConfigGeneration: a.ConfigGeneration, ChildWorkflowOrigin: &apiv1.ChildWorkflowOrigin{StageOccurrence: "occurrence", AttemptID: "attempt"}},
		Goobers: map[string]apiv1.GooberSpec{"coder": input.Goobers["coder"]}, Instructions: map[string]string{"coder": "existing parent instructions"}, SkillPackages: map[string][]workflow.SkillFile{"existing": {{Path: "SKILL.md", Content: "retained user skill"}}}}
	return a, kit
}

func catalogFromKit(t *testing.T, kit *agentickit.Kit) (AuthoringCatalog, []byte) {
	t.Helper()
	files := kit.SkillPackages[ParentAuthoringSkillName]
	if len(files) != 2 || files[0].Path != "SKILL.md" || files[1].Path != "catalog.json" {
		t.Fatal("authoring package incomplete")
	}
	var catalog AuthoringCatalog
	raw := []byte(files[1].Content)
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	return catalog, raw
}

func TestParentAuthoringKitIsBoundedScopedAndContentAddressed(t *testing.T) {
	a, kit := authoringFixture()
	originalPackages, originalGoobers, originalInstructions := kit.SkillPackages, kit.Goobers, kit.Instructions
	if err := AttachParentAuthoringSkill(kit, a); err != nil {
		t.Fatal(err)
	}
	catalog, raw := catalogFromKit(t, kit)
	if len(raw) > maxAuthoringCatalogBytes || catalog.Gaggle != "web" || catalog.DSLVersion != "3.1" || catalog.ConfigDigest != a.Origin.ConfigDigest || catalog.PolicyDigest != a.Origin.PolicyDigest || !catalog.NewSubmissions {
		t.Fatal("catalog lost bounded authority provenance", catalog)
	}
	if len(catalog.Goobers) != 1 || catalog.Goobers[0].Name != "coder" || len(catalog.Runners) != 1 || catalog.Runners[0].Name != "isolated" || !reflect.DeepEqual(catalog.AllowedCapabilities, []string{"agent:model"}) || catalog.AllowPRPublication {
		t.Fatal("catalog widened the delegation", catalog)
	}
	for _, forbidden := range []string{"private-model-marker", "private-instructions-marker", "private-image-marker", "private-command-marker", "foreign-goober-marker"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatal("catalog disclosed private configuration", forbidden)
		}
	}
	if _, exists := originalPackages[ParentAuthoringSkillName]; exists || slices.Contains(originalGoobers["coder"].Skills, ParentAuthoringSkillName) || originalInstructions["coder"] != "existing parent instructions" {
		t.Fatal("attachment mutated caller-owned configuration")
	}
	data, digest, err := agentickit.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := agentickit.Unmarshal(data, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.SkillPackages, kit.SkillPackages) {
		t.Fatal("authoring package lost in verified transport")
	}
	changed := bytes.Replace(data, []byte("child-work"), []byte("different-work"), 1)
	if _, err := agentickit.Unmarshal(changed, digest); err == nil {
		t.Fatal("tampered catalog accepted")
	}
}

func TestParentAuthoringCatalogNarrowsWithoutPreventingExistingChildObservation(t *testing.T) {
	a, kit := authoringFixture()
	a.Admission.ParentTask.ChildWorkflows.AllowPRPublication = true
	a.Admission.AllowPRPublication = true
	a.Admission.ParentTask.ChildWorkflows.AllowedCapabilities = []string{"agent:model", "repo:push"}
	a.Admission.GrantedCapabilities = []string{"agent:model"}
	a.Admission.ExecutionRefusal = "current policy withdrawn"
	delete(a.Admission.Goobers, "coder")
	if err := AttachParentAuthoringSkill(kit, a); err != nil {
		t.Fatal(err)
	}
	catalog, _ := catalogFromKit(t, kit)
	if catalog.NewSubmissions || len(catalog.Goobers) != 0 || slices.Contains(catalog.AllowedCapabilities, "repo:push") {
		t.Fatal("catalog restored removed permission", catalog)
	}
	if !strings.Contains(kit.SkillPackages[ParentAuthoringSkillName][0].Content, "inspect or reconcile existing children") {
		t.Fatal("withdrawn permission lost the custody guidance")
	}
}

func TestParentAuthoringRefusesWrongScopeCollisionAndOversizeWithoutMutation(t *testing.T) {
	for name, change := range map[string]func(*Authority, *agentickit.Kit){
		"run":        func(_ *Authority, k *agentickit.Kit) { k.Envelope.RunID = "another-run" },
		"gaggle":     func(_ *Authority, k *agentickit.Kit) { k.Envelope.Gaggle = "other" },
		"attempt":    func(_ *Authority, k *agentickit.Kit) { k.Envelope.ChildWorkflowOrigin.AttemptID = "obsolete" },
		"generation": func(_ *Authority, k *agentickit.Kit) { k.Envelope.ConfigGeneration = "different" },
		"collision": func(_ *Authority, k *agentickit.Kit) {
			k.SkillPackages[ParentAuthoringSkillName] = []workflow.SkillFile{{Path: "SKILL.md", Content: "user-owned"}}
		},
		"declared collision": func(_ *Authority, k *agentickit.Kit) {
			s := k.Goobers["coder"]
			s.Skills = []string{ParentAuthoringSkillName}
			k.Goobers["coder"] = s
		},
		"oversize": func(a *Authority, _ *agentickit.Kit) {
			a.Admission.Config.Runners[0].Provides.Capabilities = []string{strings.Repeat("x", maxAuthoringCatalogBytes)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, kit := authoringFixture()
			change(&a, kit)
			before, _, err := agentickit.Marshal(kit)
			if err != nil {
				t.Fatal(err)
			}
			if err := AttachParentAuthoringSkill(kit, a); err == nil {
				t.Fatal("invalid authoring attachment accepted")
			}
			after, _, err := agentickit.Marshal(kit)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refused attachment partially changed kit", err)
			}
		})
	}
}
