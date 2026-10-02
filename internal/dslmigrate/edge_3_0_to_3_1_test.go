package dslmigrate_test

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	k8syaml "sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/workflow"
)

func TestMigrateV30ToV31Golden(t *testing.T) {
	for _, name := range []string{"legacy-artifacts", "undeclared-outputs"} {
		t.Run(name, func(t *testing.T) {
			before, err := os.ReadFile("testdata/v3_0/" + name + ".yaml")
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile("testdata/v3_1/" + name + ".yaml")
			if err != nil {
				t.Fatal(err)
			}
			result, err := dslmigrate.Migrate(before, "3.1")
			if err != nil {
				t.Fatal(err)
			}
			if result.Before != string(before) || result.After != string(want) || !result.Changed {
				t.Fatalf("migration differs from golden: %+v", result)
			}
			if len(result.Notes) != 0 {
				t.Fatalf("pin-only migration has transform notes: %v", result.Notes)
			}
			var specs []apiv1.WorkflowSpec
			for _, source := range [][]byte{before, []byte(result.After)} {
				var wf apiv1.Workflow
				if err := k8syaml.UnmarshalStrict(source, &wf); err != nil {
					t.Fatal(err)
				}
				machine, err := workflow.Compile(workflow.Definition{Name: wf.Name, DSLVersion: wf.DSLVersion, Spec: wf.Spec})
				if err != nil {
					t.Fatalf("compile DSL %s: %v", wf.DSLVersion, err)
				}
				for _, task := range machine.Def.Spec.Tasks {
					if task.ArtifactSlots != nil || task.ArtifactInputs != nil || len(machine.ArtifactBindings(task.Name)) != 0 {
						t.Fatalf("migration inferred a semantic artifact contract: %+v", task)
					}
				}
				specs = append(specs, machine.Def.Spec)
			}
			if !reflect.DeepEqual(specs[0], specs[1]) {
				t.Fatalf("compiled workflow spec changed: before=%+v after=%+v", specs[0], specs[1])
			}
			if _, err := dslmigrate.Migrate([]byte(result.After), "3.1"); !errors.Is(err, dslmigrate.ErrAlreadyAtTarget) {
				t.Fatalf("second migration = %v, want already at target", err)
			}
		})
	}
}

func TestMigrateV31PreservesSourceFormatting(t *testing.T) {
	for _, pin := range []string{`3.0`, `"3.0"`, `'3.0'`} {
		for _, eol := range []string{"\n", "\r\n"} {
			source := "# untouched π\n{kind: Workflow, dslVersion: " + pin + ", spec: {tasks: []}} # tail\n"
			source = strings.ReplaceAll(source, "\n", eol)
			result, err := dslmigrate.Migrate([]byte(source), "3.1")
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.Replace(source, "3.0", "3.1", 1); result.After != want {
				t.Fatalf("source formatting changed:\n got %q\nwant %q", result.After, want)
			}
		}
	}
}

func TestMigrateV31RequiresAdjacentHop(t *testing.T) {
	for _, from := range []string{"1.4", "2.0", "3.2"} {
		_, err := dslmigrate.Migrate([]byte("kind: Workflow\ndslVersion: \""+from+"\"\n"), "3.1")
		if err == nil || !strings.Contains(err.Error(), "no direct migration registered") {
			t.Fatalf("%s → 3.1: %v, want no direct edge", from, err)
		}
	}
}
