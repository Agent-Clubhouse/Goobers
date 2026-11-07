// Package workflowgym loads and validates deterministic benchmark task registries.
package workflowgym

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/strictyaml"
)

var (
	taskSchemaOnce sync.Once
	taskSchema     *jsonschema.Schema
	taskSchemaErr  error
)

// Registry is a validated, deterministically ordered Workflow Gym dataset.
type Registry struct {
	DatasetVersion string
	Digest         string
	Tasks          []apiv1.WorkflowGymTaskManifest
}

// Load reads a task manifest file or a directory of task manifests.
func Load(path string) (Registry, error) {
	files, err := manifestFiles(path)
	if err != nil {
		return Registry{}, err
	}

	tasks := make([]apiv1.WorkflowGymTaskManifest, 0, len(files))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return Registry{}, fmt.Errorf("read %s: %w", file, err)
		}
		var task apiv1.WorkflowGymTaskManifest
		if err := decodeTask(raw, &task); err != nil {
			return Registry{}, fmt.Errorf("decode %s: %w", file, err)
		}
		if err := validateTask(task); err != nil {
			return Registry{}, fmt.Errorf("%s: %w", file, err)
		}
		tasks = append(tasks, task)
	}

	sort.Slice(tasks, func(i, j int) bool { return tasks[i].TaskID < tasks[j].TaskID })
	for i := 1; i < len(tasks); i++ {
		if tasks[i-1].TaskID == tasks[i].TaskID {
			return Registry{}, fmt.Errorf("duplicate task ID %q", tasks[i].TaskID)
		}
	}
	version := tasks[0].DatasetVersion
	for _, task := range tasks[1:] {
		if task.DatasetVersion != version {
			return Registry{}, fmt.Errorf("task %q uses dataset version %q, want %q", task.TaskID, task.DatasetVersion, version)
		}
	}

	canonical, err := json.Marshal(struct {
		DatasetVersion string                          `json:"datasetVersion"`
		Tasks          []apiv1.WorkflowGymTaskManifest `json:"tasks"`
	}{DatasetVersion: version, Tasks: tasks})
	if err != nil {
		return Registry{}, fmt.Errorf("encode canonical dataset: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return Registry{
		DatasetVersion: version,
		Digest:         "sha256:" + hex.EncodeToString(sum[:]),
		Tasks:          tasks,
	}, nil
}

func manifestFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read directory %s: %w", path, err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !isManifestName(entry.Name()) {
			continue
		}
		files = append(files, filepath.Join(path, entry.Name()))
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: no .json, .yaml, or .yml task manifests", path)
	}
	return files, nil
}

func isManifestName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json", ".yaml", ".yml":
		return true
	default:
		return false
	}
}

func validateTask(task apiv1.WorkflowGymTaskManifest) error {
	if task.APIVersion != apiv1.WorkflowGymTaskAPIVersion {
		return fmt.Errorf("apiVersion must be %q", apiv1.WorkflowGymTaskAPIVersion)
	}
	if task.Kind != apiv1.WorkflowGymTaskKind {
		return fmt.Errorf("kind must be %q", apiv1.WorkflowGymTaskKind)
	}
	required := []struct {
		name  string
		value string
	}{
		{"taskId", task.TaskID},
		{"datasetVersion", task.DatasetVersion},
		{"provenance.source", task.Provenance.Source},
		{"provenance.revision", task.Provenance.Revision},
		{"expectedOutcome", task.ExpectedOutcome},
		{"evaluatorVersion", task.EvaluatorVersion},
		{"retention.policy", task.Retention.Policy},
		{"licensing.license", task.Licensing.License},
		{"candidateEnvironment.image", task.CandidateEnvironment.Image},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	switch task.Split {
	case apiv1.WorkflowGymSplitTrain, apiv1.WorkflowGymSplitEval, apiv1.WorkflowGymSplitHoldout:
	default:
		return fmt.Errorf("split must be train, eval, or holdout")
	}
	generated := task.GeneratorVersion != "" || task.TaskClass != "" || len(task.Constraints) != 0 || task.MinimalReproductionID != ""
	if generated {
		for _, field := range []struct {
			name  string
			value string
		}{
			{"generatorVersion", task.GeneratorVersion},
			{"taskClass", task.TaskClass},
			{"minimalReproductionId", task.MinimalReproductionID},
		} {
			if strings.TrimSpace(field.value) == "" {
				return fmt.Errorf("%s is required for generated tasks", field.name)
			}
		}
		if len(task.Constraints) == 0 {
			return fmt.Errorf("constraints are required for generated tasks")
		}
		if !validTaskClass(TaskClass(task.TaskClass)) {
			return fmt.Errorf("taskClass %q is not supported", task.TaskClass)
		}
	}
	return nil
}

func decodeTask(raw []byte, task *apiv1.WorkflowGymTaskManifest) error {
	jsonData, err := strictyaml.YAMLToJSON(raw)
	if err != nil {
		return err
	}
	var document any
	if err := json.Unmarshal(jsonData, &document); err != nil {
		return err
	}
	schema, err := workflowGymTaskSchema()
	if err != nil {
		return err
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("schema validation: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(task); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("manifest contains multiple JSON values")
		}
		return err
	}
	return nil
}

func workflowGymTaskSchema() (*jsonschema.Schema, error) {
	taskSchemaOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		compiler.Draft = jsonschema.Draft2020
		data, err := schemas.FS.ReadFile(schemas.WorkflowGymTask)
		if err != nil {
			taskSchemaErr = fmt.Errorf("read embedded task schema: %w", err)
			return
		}
		if err := compiler.AddResource(schemas.BaseURI+schemas.WorkflowGymTask, bytes.NewReader(data)); err != nil {
			taskSchemaErr = fmt.Errorf("add task schema: %w", err)
			return
		}
		taskSchema, taskSchemaErr = compiler.Compile(schemas.BaseURI + schemas.WorkflowGymTask)
		if taskSchemaErr != nil {
			taskSchemaErr = fmt.Errorf("compile task schema: %w", taskSchemaErr)
		}
	})
	return taskSchema, taskSchemaErr
}
