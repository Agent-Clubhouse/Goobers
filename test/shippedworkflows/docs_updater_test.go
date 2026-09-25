package shippedworkflows

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

type docsUpdaterConfig struct {
	workflow apiv1.Workflow
	goober   apiv1.Goober
}

func TestDocsUpdaterShippedCopiesStayInSync(t *testing.T) {
	root := repositoryRoot(t)
	referenceWorkflows := loadDocsUpdaterConfig(t, filepath.Join(root, "reference-workflows"), "goobers")
	example := loadDocsUpdaterConfig(t, filepath.Join(root, "config-examples"), "acme-web")

	referenceWorkflows.workflow.Spec.Gaggle = ""
	example.workflow.Spec.Gaggle = ""
	normalizeDocsValidationCommand(&referenceWorkflows.workflow)
	normalizeDocsValidationCommand(&example.workflow)
	if !reflect.DeepEqual(referenceWorkflows.workflow.Spec, example.workflow.Spec) {
		t.Fatalf("reference workflows and config-example docs-updater specs drifted")
	}

	referenceWorkflows.goober.Spec.Gaggle = ""
	example.goober.Spec.Gaggle = ""
	if !reflect.DeepEqual(referenceWorkflows.goober.Spec, example.goober.Spec) {
		t.Fatalf("reference workflows and config-example docs goober specs drifted")
	}

	referenceInstructions, err := os.ReadFile(filepath.Join(
		root, "reference-workflows", "gaggles", "goobers", "goobers", "docs", "instructions.md",
	))
	if err != nil {
		t.Fatal(err)
	}
	exampleInstructions, err := os.ReadFile(filepath.Join(
		root, "config-examples", "gaggles", "acme-web", "goobers", "docs", "instructions.md",
	))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(referenceInstructions, exampleInstructions) {
		t.Fatalf("reference workflows and config-example docs instructions drifted")
	}
}

func TestDocsUpdaterWorkflowContract(t *testing.T) {
	root := repositoryRoot(t)
	configs := []struct {
		name   string
		path   string
		gaggle string
	}{
		{name: "reference-workflows", path: filepath.Join(root, "reference-workflows"), gaggle: "goobers"},
		{name: "config-example", path: filepath.Join(root, "config-examples"), gaggle: "acme-web"},
	}
	for _, config := range configs {
		config := config
		t.Run(config.name, func(t *testing.T) {
			loaded := loadDocsUpdaterConfig(t, config.path, config.gaggle)
			spec := loaded.workflow.Spec

			if !reflect.DeepEqual(spec.Triggers, []apiv1.Trigger{{Type: apiv1.TriggerManual}}) {
				t.Fatalf("triggers = %+v, want manual-only with no live schedule", spec.Triggers)
			}
			if got, want := spec.DocsRoots, []string{"docs", "README.md"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("docsRoots = %v, want %v", got, want)
			}
			if spec.Start != "signal-gather" {
				t.Fatalf("start = %q, want signal-gather", spec.Start)
			}

			gather := requireTask(t, spec, "signal-gather")
			update := requireTask(t, spec, "update-docs")
			validate := requireTask(t, spec, "validate")
			push := requireTask(t, spec, "push-branch")
			open := requireTask(t, spec, "open-pr")
			if gather.Next != "update-docs" || update.Next != "validate" ||
				validate.Next != "docs-valid" || push.Next != "open-pr" || open.Next != "" {
				t.Fatalf("unexpected docs-updater stage wiring")
			}
			rootsInput := strings.Join(spec.DocsRoots, ",")
			if gather.Inputs["resultFile"] != "docs-churn.json" || gather.Inputs["docsRoots"] != rootsInput {
				t.Fatalf("signal-gather inputs = %v, want result artifact and configured roots", gather.Inputs)
			}
			if update.Goober != "docs" ||
				!reflect.DeepEqual(update.Capabilities, []string{"repo:push", "agent:model"}) ||
				!reflect.DeepEqual(update.PolicyActions, []string{"modify-repository"}) ||
				update.Inputs["docsRoots"] != rootsInput {
				t.Fatalf("update-docs contract = %+v", update)
			}
			if !reflect.DeepEqual(loaded.goober.Spec.Capabilities, []string{"repo:push", "agent:model"}) ||
				!reflect.DeepEqual(loaded.goober.Spec.PolicyActions, []string{"modify-repository"}) {
				t.Fatalf("docs goober grants are not least-privilege: %+v", loaded.goober.Spec)
			}
			if !reflect.DeepEqual(push.Capabilities, []string{"repo:push"}) {
				t.Fatalf("push-branch capabilities = %v", push.Capabilities)
			}
			if open.Inputs["confineToDocsRoots"] != "true" || open.Inputs["docsRoots"] != rootsInput ||
				!reflect.DeepEqual(open.Capabilities, []string{"provider:pr:write"}) ||
				!contains(open.ExpectedOutputs, "pull-request-url") {
				t.Fatalf("open-pr docs confinement/publication contract = %+v", open)
			}
			gate := requireGate(t, spec, "docs-valid")
			if gate.Branches["pass"] != "push-branch" || gate.Branches["fail"] != workflow.TargetAbort {
				t.Fatalf("docs-valid branches = %v", gate.Branches)
			}
		})
	}
}

func TestMergeReviewSelectsConfiguredAutomatedReviewPrefixes(t *testing.T) {
	root := repositoryRoot(t)
	configs := []struct {
		path   string
		gaggle string
	}{
		{path: filepath.Join(root, "reference-workflows"), gaggle: "goobers"},
		{path: filepath.Join(root, "config-examples"), gaggle: "acme-web"},
	}
	for _, config := range configs {
		set, report, err := instance.LoadConfigDir(config.path)
		if err != nil {
			t.Fatalf("load %s: %v\n%v", config.path, err, report)
		}
		mergeReview := findWorkflow(t, set.Workflows, config.gaggle, "merge-review")
		selectTask := requireTask(t, mergeReview.Spec, "pr-select")
		got := splitCommaList(selectTask.Inputs["headPrefixes"])
		want := []string{"goobers/implementation/", "goobers/docs-updater/"}
		if config.gaggle == "goobers" {
			want = append(want, "goobers/tutor/")
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s merge-review headPrefixes = %v, want %v", config.path, got, want)
		}
		if _, legacy := selectTask.Inputs["headPrefix"]; legacy {
			t.Fatalf("%s merge-review retained broad/legacy headPrefix", config.path)
		}
	}
}

func loadDocsUpdaterConfig(t *testing.T, path, gaggle string) docsUpdaterConfig {
	t.Helper()
	set, report, err := instance.LoadConfigDir(path)
	if err != nil {
		t.Fatalf("load %s: %v\n%v", path, err, report)
	}
	loaded := docsUpdaterConfig{workflow: findWorkflow(t, set.Workflows, gaggle, "docs-updater")}
	for _, goober := range set.Goobers {
		if goober.Spec.Gaggle == gaggle && goober.Name == "docs" {
			loaded.goober = goober
			return loaded
		}
	}
	t.Fatalf("docs goober not found for gaggle %q", gaggle)
	return docsUpdaterConfig{}
}

func findWorkflow(t *testing.T, workflows []apiv1.Workflow, gaggle, name string) apiv1.Workflow {
	t.Helper()
	for _, candidate := range workflows {
		if candidate.Spec.Gaggle == gaggle && candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("workflow %s/%s not found", gaggle, name)
	return apiv1.Workflow{}
}

func requireTask(t *testing.T, spec apiv1.WorkflowSpec, name string) apiv1.Task {
	t.Helper()
	for _, task := range spec.Tasks {
		if task.Name == name {
			return task
		}
	}
	t.Fatalf("task %q not found", name)
	return apiv1.Task{}
}

func requireGate(t *testing.T, spec apiv1.WorkflowSpec, name string) apiv1.Gate {
	t.Helper()
	for _, gate := range spec.Gates {
		if gate.Name == name {
			return gate
		}
	}
	t.Fatalf("gate %q not found", name)
	return apiv1.Gate{}
}

func normalizeDocsValidationCommand(definition *apiv1.Workflow) {
	for index := range definition.Spec.Tasks {
		task := &definition.Spec.Tasks[index]
		if task.Name != "validate" || task.Run == nil {
			continue
		}
		task.Run.Command = []string{"project-ci"}
		// expectedSubprocessTimeoutSeconds (#3377) documents the wall-clock
		// ceiling of *this repo's own* `make ci` -> `go test -timeout 30m`;
		// it does not generalize to the example's actual runtime command
		// (acme-web's gaggle.yaml overrides to `npm run ci`, MGV-1/#1009), so
		// it is legitimately reference-only and excluded from this
		// comparison the same way the command itself is normalized above.
		// Deleting down to an empty-but-non-nil map would itself drift
		// against the example's untouched nil map, so drop the key to nil
		// when it was the only entry.
		delete(task.Inputs, "expectedSubprocessTimeoutSeconds")
		if len(task.Inputs) == 0 {
			task.Inputs = nil
		}
	}
}

func splitCommaList(value string) []string {
	var values []string
	for _, candidate := range strings.Split(value, ",") {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			values = append(values, candidate)
		}
	}
	return values
}
