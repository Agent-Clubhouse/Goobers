package workflow

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func containedParallelDefinition() Definition {
	def := childPolicyDefinition()
	seed := def.Spec.Tasks[0]
	seed.Name, seed.Next, seed.Workspace = "seed", "fan", apiv1.WorkspaceRepo
	a, b, join := seed, seed, seed
	a.Name, a.Next, a.RepoFrom = "a", TargetJoin, apiv1.RepoFrom{"seed"}
	b.Name, b.Next, b.RepoFrom = "b", TargetJoin, apiv1.RepoFrom{"seed"}
	join.Name, join.Next, join.RepoFrom = "join", "", apiv1.RepoFrom{"seed", "a", "b"}
	def.Spec.Start, def.Spec.Tasks = "seed", []apiv1.Task{seed, a, b, join}
	def.Spec.Parallels = []apiv1.Parallel{{Name: "fan", Join: "join", FailurePolicy: apiv1.BranchContinueOnError, MaxConcurrentBranches: 2, Branches: []apiv1.Branch{{Name: "a", Start: "a"}, {Name: "b", Start: "b"}}}}
	return def
}

func TestChildParallelCompilerPreservesExactSource(t *testing.T) {
	def := containedParallelDefinition()
	machine, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	want, err := ComputeDigest(def)
	if err != nil {
		t.Fatal(err)
	}
	if machine.Digest() != want || !reflect.DeepEqual(machine.Def, def) {
		t.Fatal("projection became source authority")
	}
	for _, expected := range def.Spec.Tasks {
		actual, ok := machine.Task(expected.Name)
		if !ok || !reflect.DeepEqual(actual, expected) {
			t.Fatal("projected runtime task", actual)
		}
	}
	if len(machine.Graph().Nodes) != 5 {
		t.Fatal("graph topology changed")
	}
}

func TestChildParallelCompilerOnlyOverridesExactWorkspaceRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Definition)
		want   string
	}{
		{"handoff", func(d *Definition) { d.Spec.Tasks[3].RepoFrom = apiv1.RepoFrom{"a"} }, "repoFrom"},
		{"capability", func(d *Definition) { d.Spec.Tasks[1].Capabilities = []string{"invalid:grant"} }, "capability"},
		{"topology", func(d *Definition) { d.Spec.Tasks[1].Next = "missing" }, "missing"},
		{"ordinary branch", func(d *Definition) { d.Spec.Tasks[1].ChildWorkflows = nil }, "writable repo"},
		{"old dialect", func(d *Definition) {
			d.DSLVersion = "3.0"
			for i := range d.Spec.Tasks {
				d.Spec.Tasks[i].ChildWorkflows = nil
			}
		}, "writable repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := containedParallelDefinition()
			tc.change(&d)
			_, err := Compile(d, WithPreviewFeatures(true), WithGoobers(map[string]apiv1.GooberSpec{"coder": {Capabilities: []string{"repo:read"}}}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
		})
	}
}
