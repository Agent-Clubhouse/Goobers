package instance

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func gaggle(name string, spec apiv1.GaggleSpec) apiv1.Gaggle {
	g := apiv1.Gaggle{Spec: spec}
	g.Name = name
	return g
}

func workflowWithLocalCI(name, gaggle string, ciCommand []string) apiv1.Workflow {
	wf := apiv1.Workflow{
		Spec: apiv1.WorkflowSpec{
			Gaggle: gaggle,
			Tasks: []apiv1.Task{
				{Name: "implement", Type: apiv1.TaskAgentic, Goal: "do", Goober: "impl"},
				{Name: LocalCIStageName, Type: apiv1.TaskDeterministic, Goal: "ci", Run: &apiv1.DeterministicRun{Command: ciCommand}},
			},
		},
	}
	wf.Name = name
	return wf
}

func TestApplyGaggleCICommand_OverridesLocalCI(t *testing.T) {
	set := &ConfigSet{
		Gaggles:   []apiv1.Gaggle{gaggle("web", apiv1.GaggleSpec{CICommand: []string{"npm", "run", "ci"}})},
		Workflows: []apiv1.Workflow{workflowWithLocalCI("impl", "web", []string{"make", "ci"})},
	}
	ApplyGaggleCICommand(set)

	got := set.Workflows[0].Spec.Tasks[1].Run.Command
	if want := []string{"npm", "run", "ci"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("local-ci command = %v, want %v", got, want)
	}
	// The agentic stage is untouched.
	if set.Workflows[0].Spec.Tasks[0].Run != nil {
		t.Errorf("non-CI stage was mutated")
	}
}

func TestApplyGaggleCICommand_NoOverrideKeepsDeclaredCommand(t *testing.T) {
	// A gaggle with no CICommand leaves the declared default in place — the
	// single-Go-gaggle regression case.
	set := &ConfigSet{
		Gaggles:   []apiv1.Gaggle{gaggle("goobers", apiv1.GaggleSpec{})},
		Workflows: []apiv1.Workflow{workflowWithLocalCI("impl", "goobers", []string{"make", "ci"})},
	}
	ApplyGaggleCICommand(set)

	if got, want := set.Workflows[0].Spec.Tasks[1].Run.Command, []string{"make", "ci"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("local-ci command = %v, want unchanged %v", got, want)
	}
}

func TestApplyGaggleCICommand_OnlyRewritesMatchingGaggleAndStage(t *testing.T) {
	set := &ConfigSet{
		Gaggles: []apiv1.Gaggle{gaggle("web", apiv1.GaggleSpec{CICommand: []string{"npm", "run", "ci"}})},
		Workflows: []apiv1.Workflow{
			workflowWithLocalCI("web-impl", "web", []string{"make", "ci"}),
			workflowWithLocalCI("go-impl", "goobers", []string{"make", "ci"}), // different gaggle
		},
	}
	ApplyGaggleCICommand(set)

	if got, want := set.Workflows[0].Spec.Tasks[1].Run.Command, []string{"npm", "run", "ci"}; !reflect.DeepEqual(got, want) {
		t.Errorf("web workflow local-ci = %v, want %v", got, want)
	}
	if got, want := set.Workflows[1].Spec.Tasks[1].Run.Command, []string{"make", "ci"}; !reflect.DeepEqual(got, want) {
		t.Errorf("other-gaggle workflow local-ci = %v, want unchanged %v", got, want)
	}
}

func TestApplyGaggleCICommand_IgnoresNonLocalCIDeterministicStage(t *testing.T) {
	// A deterministic stage that is not named local-ci keeps its command even
	// when the gaggle declares a CI command.
	wf := apiv1.Workflow{
		Spec: apiv1.WorkflowSpec{
			Gaggle: "web",
			Tasks: []apiv1.Task{
				{Name: "push-branch", Type: apiv1.TaskDeterministic, Goal: "push", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "push-branch"}}},
			},
		},
	}
	wf.Name = "impl"
	set := &ConfigSet{
		Gaggles:   []apiv1.Gaggle{gaggle("web", apiv1.GaggleSpec{CICommand: []string{"npm", "run", "ci"}})},
		Workflows: []apiv1.Workflow{wf},
	}
	ApplyGaggleCICommand(set)

	if got, want := set.Workflows[0].Spec.Tasks[0].Run.Command, []string{"goobers", "push-branch"}; !reflect.DeepEqual(got, want) {
		t.Errorf("push-branch command = %v, want unchanged %v", got, want)
	}
}

func TestApplyGaggleCICommand_CopiesSoNoAliasing(t *testing.T) {
	ci := []string{"npm", "run", "ci"}
	set := &ConfigSet{
		Gaggles: []apiv1.Gaggle{gaggle("web", apiv1.GaggleSpec{CICommand: ci})},
		Workflows: []apiv1.Workflow{
			workflowWithLocalCI("a", "web", []string{"make", "ci"}),
			workflowWithLocalCI("b", "web", []string{"make", "ci"}),
		},
	}
	ApplyGaggleCICommand(set)
	// Mutating one workflow's resolved command must not disturb the gaggle
	// source or the sibling workflow.
	set.Workflows[0].Spec.Tasks[1].Run.Command[0] = "MUTATED"
	if ci[0] != "npm" {
		t.Errorf("gaggle CICommand aliased and mutated: %v", ci)
	}
	if got := set.Workflows[1].Spec.Tasks[1].Run.Command[0]; got != "npm" {
		t.Errorf("sibling workflow aliased: %v", got)
	}
}

func TestWorkflowRequiredCapabilities_UnionGaggleAndStages(t *testing.T) {
	g := gaggle("web", apiv1.GaggleSpec{RequiredCapabilities: []string{"os=linux"}})
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks: []apiv1.Task{
			{Name: "build", Type: apiv1.TaskDeterministic, Goal: "b", Run: &apiv1.DeterministicRun{Command: []string{"x"}}, RequiredCapabilities: []string{"dotnet@8"}},
			{Name: "ui", Type: apiv1.TaskDeterministic, Goal: "u", Run: &apiv1.DeterministicRun{Command: []string{"y"}}, RequiredCapabilities: []string{"node@20", "dotnet@8"}},
		},
	}}
	wf.Name = "impl"

	got := WorkflowRequiredCapabilities(g, wf)
	want := []string{"dotnet@8", "node@20", "os=linux"} // sorted, de-duped
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WorkflowRequiredCapabilities = %v, want %v", got, want)
	}
}

func TestWorkflowRequiredCapabilities_NoneIsNil(t *testing.T) {
	g := gaggle("goobers", apiv1.GaggleSpec{})
	wf := workflowWithLocalCI("impl", "goobers", []string{"make", "ci"})
	if got := WorkflowRequiredCapabilities(g, wf); got != nil {
		t.Errorf("WorkflowRequiredCapabilities = %v, want nil", got)
	}
}

func TestCheckCapabilityRequirements(t *testing.T) {
	webGaggle := gaggle("web", apiv1.GaggleSpec{RequiredCapabilities: []string{"dotnet@10"}})
	stageReqWF := func() apiv1.Workflow {
		wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Gaggle: "web",
			Tasks: []apiv1.Task{
				{Name: "build", Type: apiv1.TaskDeterministic, Goal: "b", Run: &apiv1.DeterministicRun{Command: []string{"x"}}, RequiredCapabilities: []string{"xcode"}},
			},
		}}
		wf.Name = "impl"
		return wf
	}

	t.Run("met requirement passes", func(t *testing.T) {
		set := &ConfigSet{Gaggles: []apiv1.Gaggle{webGaggle}, Workflows: []apiv1.Workflow{stageReqWF()}}
		if err := CheckCapabilityRequirements([]string{"dotnet@10", "xcode"}, set); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unmet gaggle requirement rejected, names capability", func(t *testing.T) {
		set := &ConfigSet{Gaggles: []apiv1.Gaggle{webGaggle}, Workflows: []apiv1.Workflow{stageReqWF()}}
		err := CheckCapabilityRequirements([]string{"xcode"}, set) // missing dotnet@10
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "dotnet@10") || !strings.Contains(err.Error(), "web") {
			t.Errorf("error must name the missing capability and gaggle: %v", err)
		}
	})

	t.Run("unmet stage requirement rejected", func(t *testing.T) {
		set := &ConfigSet{Gaggles: []apiv1.Gaggle{webGaggle}, Workflows: []apiv1.Workflow{stageReqWF()}}
		err := CheckCapabilityRequirements([]string{"dotnet@10"}, set) // missing xcode
		if err == nil || !strings.Contains(err.Error(), "xcode") {
			t.Errorf("expected error naming xcode, got %v", err)
		}
	})

	t.Run("no requirements passes with no runner capabilities", func(t *testing.T) {
		set := &ConfigSet{
			Gaggles:   []apiv1.Gaggle{gaggle("goobers", apiv1.GaggleSpec{})},
			Workflows: []apiv1.Workflow{workflowWithLocalCI("impl", "goobers", []string{"make", "ci"})},
		}
		if err := CheckCapabilityRequirements(nil, set); err != nil {
			t.Fatalf("unexpected error for a no-requirements instance: %v", err)
		}
	})

	t.Run("nil set is a no-op", func(t *testing.T) {
		if err := CheckCapabilityRequirements([]string{"dotnet@8"}, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
