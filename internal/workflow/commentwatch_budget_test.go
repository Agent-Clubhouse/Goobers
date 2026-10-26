package workflow

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

func commentWatcherDefinition(t *testing.T, version string) Definition {
	t.Helper()
	raw, err := os.ReadFile("../../cmd/goobers/testdata/pr-comment-watch.workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var resource apiv1.Workflow
	if err := yaml.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	return Definition{Name: resource.Name, Version: 1, DSLVersion: version, Spec: resource.Spec}
}

func TestCommentWatchBudgetPermissionIsVersioned(t *testing.T) {
	for _, version := range []string{"2.0", "2.1", "3.0", "3.1"} {
		for _, provider := range []string{"github", "gitea", "ado"} {
			t.Run(version+"/"+provider, func(t *testing.T) {
				def := commentWatcherDefinition(t, version)
				task := &def.Spec.Tasks[0]
				// Shared compiler contract with each provider's credential shape. This
				// exercises compilation, not external provider execution.
				task.Inputs = map[string]string{"resultFile": "comment-watch-result.json"}
				task.PolicyActions = []string{"label-issue", "comment-on-issue"}
				task.Capabilities = []string{"github:issues:write"}
				if provider == "ado" {
					task.Capabilities = append(task.Capabilities, "github:pr:write")
				}
				machine, err := Compile(def)
				if err != nil {
					t.Fatal(err)
				}
				if machine.Def.DSLVersion != version {
					t.Fatalf("compiled pin = %q, want %q", machine.Def.DSLVersion, version)
				}
				task.PolicyActions = []string{"label-issue"}
				before := slices.Clone(task.PolicyActions)
				_, err = Compile(def)
				admission := strings.Join(CheckWorkflowAdmission(def, nil), "; ")
				if version == "2.0" {
					if err != nil {
						t.Fatalf("unchanged legacy permission rejected: %v", err)
					}
					if admission != "" {
						t.Fatalf("legacy admission: %s", admission)
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), "comment-on-issue") {
						t.Fatalf("missing comment permission: %v", err)
					}
					if !strings.Contains(admission, "comment-on-issue") {
						t.Fatalf("admission missed comment permission: %s", admission)
					}
				}
				if !reflect.DeepEqual(task.PolicyActions, before) {
					t.Fatal("compiler mutated source permissions")
				}
			})
		}
	}
}

func TestCommentWatchIndependentPermissionsRemainRequired(t *testing.T) {
	for _, version := range []string{"2.0", "2.1", "3.0", "3.1"} {
		for _, action := range []string{"label-issue", "disarm-landing"} {
			t.Run(version+"/"+action, func(t *testing.T) {
				def := commentWatcherDefinition(t, version)
				def.Spec.Tasks[0].PolicyActions = slices.DeleteFunc(def.Spec.Tasks[0].PolicyActions, func(value string) bool { return value == action })
				if _, err := Compile(def); err == nil || !strings.Contains(err.Error(), action) {
					t.Fatalf("missing %s: %v", action, err)
				}
			})
		}
	}
}

func TestCommentWatchPermissionDoesNotReplaceCapability(t *testing.T) {
	def := commentWatcherDefinition(t, "2.1")
	def.Spec.Tasks[0].Capabilities = []string{"github:pr:write"}
	if _, err := Compile(def); err == nil || !strings.Contains(err.Error(), "github:issues:write") {
		t.Fatalf("missing capability admitted: %v", err)
	}
}
