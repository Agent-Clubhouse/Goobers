package workflow

import (
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func childSelectionDefinition() Definition {
	def := childPolicyDefinition()
	p := def.Spec.Tasks[0].ChildWorkflows
	p.AllowedAgentProfiles = []apiv1.ChildAgentProfile{{Name: "fast", Goober: "coder", Harness: apiv1.HarnessCopilot, Model: "configured-model"}}
	p.AllowedBases = []apiv1.ChildBaseReference{{Name: "integration", Ref: "refs/heads/integration"}}
	return def
}

func TestChildSelectionDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*apiv1.ChildWorkflowPolicy)
		want string
	}{
		{"valid", func(*apiv1.ChildWorkflowPolicy) {}, ""},
		{"profile name", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedAgentProfiles[0].Name = "../bad" }, "unique names"},
		{"duplicate profile", func(p *apiv1.ChildWorkflowPolicy) {
			p.AllowedAgentProfiles = append(p.AllowedAgentProfiles, p.AllowedAgentProfiles[0])
		}, "unique names"},
		{"duplicate tuple", func(p *apiv1.ChildWorkflowPolicy) {
			p.AllowedAgentProfiles = append(p.AllowedAgentProfiles, p.AllowedAgentProfiles[0])
			p.AllowedAgentProfiles[1].Name = "other"
		}, "duplicate goober/harness/model"},
		{"unlisted goober", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedAgentProfiles[0].Goober = "foreign" }, "allowedGoobers"},
		{"harness command", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedAgentProfiles[0].Harness = "/bin/sh" }, "harness identifier"},
		{"empty model", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedAgentProfiles[0].Model = "" }, "model identifier"},
		{"model flag", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedAgentProfiles[0].Model = "model --bypass" }, "model identifier"},
		{"model control", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedAgentProfiles[0].Model = "model\x00" }, "model identifier"},
		{"model bound", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedAgentProfiles[0].Model = strings.Repeat("a", 257) }, "model identifier"},
		{"profile bound", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedAgentProfiles = make([]apiv1.ChildAgentProfile, 33) }, "at most 32"},
		{"base bound", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedBases = make([]apiv1.ChildBaseReference, 33) }, "at most 32"},
		{"base name", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedBases[0].Name = "" }, "unique names"},
		{"duplicate base", func(p *apiv1.ChildWorkflowPolicy) { p.AllowedBases = append(p.AllowedBases, p.AllowedBases[0]) }, "unique names"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := childSelectionDefinition()
			tc.edit(def.Spec.Tasks[0].ChildWorkflows)
			goobers := map[string]apiv1.GooberSpec{"coder": {Gaggle: "web"}}
			_, err := Compile(def, WithGoobers(goobers), WithPreviewFeatures(true))
			problems := CheckWorkflowAdmission(def, goobers)
			if tc.want == "" {
				if err != nil || len(problems) != 0 {
					t.Fatalf("compile=%v admission=%v", err, problems)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(strings.Join(problems, ";"), tc.want) {
				t.Fatalf("compile=%v admission=%v; want %q", err, problems, tc.want)
			}
		})
	}
}

func TestChildSelectionBaseRefs(t *testing.T) {
	for _, ref := range []string{"refs/heads/integration", "refs/tags/v1.2.3", "refs/heads/team/topic"} {
		if !validChildBaseRef(ref) {
			t.Errorf("valid ref refused: %q", ref)
		}
	}
	for _, ref := range []string{"", "main", "HEAD", "refs/heads/", "refs/remotes/origin/main", "https://example.invalid/repo",
		"refs/heads/main~1", "refs/heads/main^", "refs/heads/a..b", "refs/heads/main@{1}", "refs/heads/a:b", "refs/heads/a?b",
		"refs/heads/a*b", "refs/heads/a[b", "refs/heads/a\\b", "refs/heads/a b", "refs/heads/a\nb", "refs/heads/a\x00b",
		"refs/heads//main", "refs/heads/.hidden", "refs/heads/main.lock", "refs/heads/main.", "refs/heads/" + strings.Repeat("a", 1024)} {
		t.Run(ref, func(t *testing.T) {
			def := childSelectionDefinition()
			def.Spec.Tasks[0].ChildWorkflows.AllowedBases[0].Ref = ref
			if _, err := Compile(def, WithPreviewFeatures(true)); err == nil || !strings.Contains(err.Error(), "fully qualified") {
				t.Fatalf("unsafe base %q: %v", ref, err)
			}
		})
	}
}

func TestChildSelectionCompilePinsAndRefusal(t *testing.T) {
	def := childSelectionDefinition()
	first, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	def.Spec.Tasks[0].ChildWorkflows.AllowedAgentProfiles[0].Model = "another-model"
	second, err := Compile(def, WithPreviewFeatures(true))
	if err != nil || first.Digest() == second.Digest() {
		t.Fatalf("model declaration not pinned: %v", err)
	}
	def.Spec.Tasks[0].ChildWorkflows.AllowedBases[0].Ref = "refs/tags/v1"
	third, err := Compile(def, WithPreviewFeatures(true))
	if err != nil || second.Digest() == third.Digest() {
		t.Fatalf("base declaration not pinned: %v", err)
	}
	if err := RefuseChildSelectionExecution(def.Spec); !errors.Is(err, ErrChildSelectionExecutionUnsupported) {
		t.Fatalf("selection execution not refused: %v", err)
	}
	if err := RefuseChildSelectionExecution(childPolicyDefinition().Spec); err != nil {
		t.Fatalf("existing child policy refused: %v", err)
	}
	for _, version := range []string{"", "2.0", "3.0"} {
		def.DSLVersion = version
		if _, err := Compile(def, WithPreviewFeatures(true)); err == nil {
			t.Fatalf("selection accepted on DSL %q", version)
		}
	}
}
