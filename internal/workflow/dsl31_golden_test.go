package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func readDSL31Golden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/dsl31/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func loadDSL31Golden(t *testing.T, name string) Definition {
	t.Helper()
	var doc apiv1.Workflow
	if err := yaml.UnmarshalStrict(readDSL31Golden(t, name), &doc); err != nil {
		t.Fatal(err)
	}
	return Definition{Name: doc.Name, DSLVersion: doc.DSLVersion, Spec: doc.Spec}
}

func TestDSL31NamedSlotGoldenWithoutExpectedOutputs(t *testing.T) {
	def := loadDSL31Golden(t, "named-slots.yaml")
	for _, task := range def.Spec.Tasks {
		if task.ExpectedOutputs != nil {
			t.Fatalf("fixture must exercise optional expectedOutputs: %+v", task)
		}
	}
	machine, err := Compile(def)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(machine.Def.Spec.Tasks[0].ArtifactSlots, def.Spec.Tasks[0].ArtifactSlots) {
		t.Fatal("compiler changed producer slots")
	}
	raw, err := json.MarshalIndent(machine.ArtifactBindings("consume"), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if want := string(readDSL31Golden(t, "named-slots.golden.json")); string(raw)+"\n" != want {
		t.Fatalf("named artifact bindings drift:\n%s\nwant:\n%s", raw, want)
	}
}

func TestDSL31DiagnosticOrderGolden(t *testing.T) {
	want := string(readDSL31Golden(t, "diagnostics.golden.txt"))
	// Repeated fresh parses exercise the consumer map's randomized iteration
	// order; the compiler must still return the same ordered source identities.
	for range 20 {
		def := loadDSL31Golden(t, "diagnostics.yaml")
		diagnostics := ArtifactContractDiagnostics(def)
		var got strings.Builder
		for _, d := range diagnostics {
			fmt.Fprintf(&got, "%s|%s|%s|%s|%s\n", d.TaskName, d.ArtifactInput, d.SlotTaskName, d.SlotName, d.Message)
		}
		if got.String() != want {
			t.Fatalf("artifact diagnostic order or identity drift:\n%swant:\n%s", got.String(), want)
		}
		_, err := Compile(def)
		var compileErr *CompileError
		if !errors.As(err, &compileErr) || !reflect.DeepEqual(compileErr.Diagnostics, diagnostics) {
			t.Fatalf("compile diagnostics differ from validation: %v", err)
		}
	}
}
