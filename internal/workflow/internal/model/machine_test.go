package model

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestMachineRuntimeLookups(t *testing.T) {
	task := apiv1.Task{Name: "task", Next: "gate"}
	gate := apiv1.Gate{Name: "gate", Branches: map[string]string{
		"pass": TerminalComplete,
		"fail": TargetAbort,
	}}
	machine, err := NewMachine(
		Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{task}, Gates: []apiv1.Gate{gate}}},
		map[string]apiv1.Task{task.Name: task},
		map[string]apiv1.Gate{gate.Name: gate},
		Graph{},
	)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := machine.Task(task.Name); !ok || !reflect.DeepEqual(got, task) {
		t.Fatalf("Task() = %+v,%v", got, ok)
	}
	if got, ok := machine.Gate(gate.Name); !ok || !reflect.DeepEqual(got, gate) {
		t.Fatalf("Gate() = %+v,%v", got, ok)
	}
	if !machine.Has(task.Name) || !machine.Has(gate.Name) || machine.Has("missing") {
		t.Fatal("Has() did not distinguish defined and missing states")
	}
	if got := machine.Outgoing(task.Name); !reflect.DeepEqual(got, []string{"gate"}) {
		t.Fatalf("task Outgoing() = %v", got)
	}
	if got := machine.Outgoing(gate.Name); !reflect.DeepEqual(got, []string{TargetAbort, TerminalComplete}) {
		t.Fatalf("gate Outgoing() = %v", got)
	}
	if got := machine.Outgoing("missing"); got != nil {
		t.Fatalf("missing Outgoing() = %v, want nil", got)
	}
}

func TestMachineDigestAndTargets(t *testing.T) {
	def := Definition{Name: "digest", Version: 1}
	machine, err := NewMachine(
		def,
		map[string]apiv1.Task{},
		map[string]apiv1.Gate{},
		Graph{},
		WithGooberDigest("sha256:goober"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if machine.Digest() != "sha256:fe1010736a8f4ff76e311232c8f7b32b6cd6a6b4d9abb94791cc76e21b4b57ae" {
		t.Fatalf("Digest() = %q", machine.Digest())
	}
	if _, ok := reflect.TypeOf(machine).MethodByName("SetDigest"); ok {
		t.Fatal("Machine exposes digest mutation")
	}
	if machine.GooberDigest() != "sha256:goober" {
		t.Fatalf("GooberDigest() = %q", machine.GooberDigest())
	}

	gate := apiv1.Gate{Branches: map[string]string{"pass": "next"}}
	if target, ok := BranchTarget(gate, "pass"); !ok || target != "next" {
		t.Fatalf("BranchTarget() = %q,%v", target, ok)
	}
	if _, ok := BranchTarget(gate, "missing"); ok {
		t.Fatal("missing branch resolved")
	}
	if !IsReservedTarget(TargetAbort) || !IsReservedTarget(TargetEscalate) {
		t.Fatal("reserved targets were not recognized")
	}
	if IsReservedTarget(TerminalComplete) || IsReservedTarget("next") {
		t.Fatal("ordinary targets were treated as reserved")
	}
}
