package workflowgym

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type generatorFixture struct {
	Class           TaskClass `json:"class"`
	FailureMode     string    `json:"failureMode"`
	ClassConstraint string    `json:"classConstraint"`
}

func TestGenerateIsDeterministicAndMatchesClassFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "generator-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []generatorFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != len(TaskClasses()) {
		t.Fatalf("fixture count = %d, want %d", len(fixtures), len(TaskClasses()))
	}

	dir := t.TempDir()
	for _, fixture := range fixtures {
		t.Run(string(fixture.Class), func(t *testing.T) {
			first, err := Generate(GeneratorVersionV1, fixture.Class, 8225)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Generate(GeneratorVersionV1, fixture.Class, 8225)
			if err != nil {
				t.Fatal(err)
			}
			firstJSON, err := json.Marshal(first)
			if err != nil {
				t.Fatal(err)
			}
			secondJSON, err := json.Marshal(second)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(firstJSON, secondJSON) {
				t.Fatalf("same version/class/seed produced different tasks:\n%s\n%s", firstJSON, secondJSON)
			}
			if first.ExpectedFailureMode != fixture.FailureMode {
				t.Fatalf("failure mode = %q, want %q", first.ExpectedFailureMode, fixture.FailureMode)
			}
			if !contains(first.Constraints, fixture.ClassConstraint) {
				t.Fatalf("constraints %v do not contain %q", first.Constraints, fixture.ClassConstraint)
			}
			if first.GeneratorVersion != GeneratorVersionV1 || first.Seed != 8225 ||
				first.MinimalReproductionID == "" {
				t.Fatalf("generator provenance is incomplete: %+v", first)
			}
			if err := os.WriteFile(filepath.Join(dir, string(fixture.Class)+".json"), firstJSON, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}

	registry, err := Load(dir)
	if err != nil {
		t.Fatalf("generated tasks do not validate through the registry: %v", err)
	}
	if len(registry.Tasks) != len(fixtures) {
		t.Fatalf("registry task count = %d, want %d", len(registry.Tasks), len(fixtures))
	}
}

func TestGenerateNeverRequestsCredentialsOrProductionSinks(t *testing.T) {
	for _, class := range TaskClasses() {
		task, err := Generate(GeneratorVersionV1, class, 1)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		serialized := strings.ToLower(string(raw))
		for _, forbidden := range []string{"github_token", "azure_devops_ext_pat", "api_key", "://github.com", "://dev.azure.com"} {
			if strings.Contains(serialized, forbidden) {
				t.Errorf("%s task contains forbidden credential or production sink %q", class, forbidden)
			}
		}
		if got := task.CandidateEnvironment.Environment["WORKFLOW_GYM_PROVIDER_URL"]; got != "https://workflow-gym.invalid" {
			t.Errorf("%s provider URL = %q", class, got)
		}
	}
}

func TestGenerateRejectsUnknownVersionAndClass(t *testing.T) {
	if _, err := Generate("v2", TaskClassNormal, 1); err == nil {
		t.Fatal("unknown generator version succeeded")
	}
	if _, err := Generate(GeneratorVersionV1, TaskClass("unknown"), 1); err == nil {
		t.Fatal("unknown task class succeeded")
	}
}

func TestRegistryRejectsIncompleteGeneratedTasks(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"missing reproduction id": func(task map[string]any) { delete(task, "minimalReproductionId") },
		"missing constraints":     func(task map[string]any) { delete(task, "constraints") },
		"unknown class":           func(task map[string]any) { task["taskClass"] = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			task, err := Generate(GeneratorVersionV1, TaskClassTimeout, 1)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			mutate(fields)
			if raw, err = json.Marshal(fields); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "task.json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); err == nil {
				t.Fatal("incomplete generated task validated")
			}
		})
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
