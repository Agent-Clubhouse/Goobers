package workflow

import (
	"os"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCommentWatchReferenceDeclaresBudgetCommentAction(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/goobers/testdata/pr-comment-watch.workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var resource apiv1.Workflow
	if err := yaml.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			def := Definition{Name: resource.Name, Version: 1, DSLVersion: version, Spec: resource.Spec}
			if _, err := Compile(def); err != nil {
				t.Fatal(err)
			}
			task := def.Spec.Tasks[0]
			task.PolicyActions = slices.DeleteFunc(slices.Clone(task.PolicyActions), func(action string) bool { return action == "comment-on-issue" })
			def.Spec.Tasks = []apiv1.Task{task}
			if _, err := Compile(def); err == nil {
				t.Fatal("comment watcher admitted without its routing-budget comment action")
			}
		})
	}
}
