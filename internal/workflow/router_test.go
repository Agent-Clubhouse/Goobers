package workflow

import (
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/supportmatrix"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
)

func TestCompileDispatchesCurrentVersion(t *testing.T) {
	def := Definition{
		Name:       "current",
		Version:    1,
		DSLVersion: supportmatrix.CurrentDSLVersion,
		Spec:       linearSpec(),
	}

	got, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want, err := vcurrent.Compile(def, vcurrent.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("v_current.Compile: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("router machine differs from v_current interpreter:\n got  %#v\n want %#v", got, want)
	}
}

func TestCompileUnpinnedUsesCurrentInterpreterWithoutChangingDefinition(t *testing.T) {
	def := Definition{Name: "unpinned", Version: 1, Spec: linearSpec()}
	machine, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if machine.Def.DSLVersion != "" {
		t.Fatalf("compiled DSL version = %q, want transitional unpinned definition preserved", machine.Def.DSLVersion)
	}
}

func TestCompileRejectsUnknownDSLVersion(t *testing.T) {
	def := Definition{Name: "unknown", Version: 1, DSLVersion: "9.9", Spec: linearSpec()}
	_, err := Compile(def, WithPreviewFeatures(true))
	if err == nil || !strings.Contains(err.Error(), `DSL version "9.9" is not supported by this build`) {
		t.Fatalf("Compile error = %v, want unsupported-version diagnostic", err)
	}
}
