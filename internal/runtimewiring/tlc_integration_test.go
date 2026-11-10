//go:build integration

package runtimewiring_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runtimewiring"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationTLCWorkflow(t *testing.T) {
	testdep.Require(t, "java")
	testdep.RequireEnv(t, "GOOBERS_TEST_TLC_JAR")
	testdep.RequireEnv(t, "GOOBERS_TEST_JAVA_HOME")
	machine := compileTLCExample(t)
	task, ok := machine.Task("verify-model")
	if !ok || task.Run == nil {
		t.Fatal("documented workflow lost its deterministic checker task")
	}
	root, err := os.MkdirTemp(".", ".tlc-workflow-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	reviewed, candidate, scratch := prepareTLCWorkflow(t, root)
	jr, err := journal.Create(filepath.Join(root, "runs"), journal.RunIdentity{RunID: "tlc-workflow", Workflow: machine.Def.Name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	// No executable named "tlc" (or even java) can be found through PATH.
	t.Setenv("PATH", "")
	deterministic, err := runtimewiring.BuildDeterministicExecutor(runtimewiring.DeterministicExecutorInput{
		Config: &instance.Config{}, Resolver: resolver, ArtifactRecorder: jr,
		ConfigDirectory: reviewed, ScratchDir: scratch,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := engine.Activities{Det: deterministic, Workspaces: tlcWorkflowWorkspace{path: candidate}}
	resolved, err := wf.TaskInvocationInputs(machine, task)
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]any{}
	for key, value := range resolved {
		inputs[key] = value
	}
	env := apiv1.InvocationEnvelope{RunID: "tlc-workflow", TaskID: task.Name, Attempt: 1, Inputs: inputs, Capabilities: task.Capabilities}
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: task.Name, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	result, err := activity.RunDeterministic(t.Context(), env, *task.Run, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != apiv1.ResultSuccess || result.Outputs["verificationOutcome"] != string(creditgraph.VerificationProven) {
		t.Fatalf("compiled workflow did not invoke TLC: %+v", result)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: task.Name, Attempt: 1, Status: string(result.Status)}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(jr.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := creditgraph.Build(creditgraph.Input{RunID: env.RunID, Events: events})
	if err != nil || len(graph.Verifications) != 1 || graph.Verifications[0].Outcome != creditgraph.VerificationProven {
		t.Fatalf("compiled workflow checker evidence was not accepted: %+v, %v", graph, err)
	}
	t.Logf("schema/compiler -> production registration -> engine activity -> actual TLC -> journal/Backprop: %+v", graph.Verifications[0])
}

func compileTLCExample(t *testing.T) *wf.Machine {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "backprop-tlc", "workflow.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.YAMLToJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateJSON("workflow.schema.json", encoded); err != nil {
		t.Fatal(err)
	}
	var workflow apiv1.Workflow
	if err := json.Unmarshal(encoded, &workflow); err != nil {
		t.Fatal(err)
	}
	machine, err := wf.Compile(wf.Definition{Name: workflow.Name, Version: 1, DSLVersion: workflow.DSLVersion, Annotations: workflow.Annotations, Spec: workflow.Spec})
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func prepareTLCWorkflow(t *testing.T, root string) (string, string, string) {
	t.Helper()
	reviewed, candidate, scratch := filepath.Join(root, "reviewed"), filepath.Join(root, "candidate"), filepath.Join(root, "scratch")
	example := filepath.Join("..", "..", "examples", "backprop-tlc")
	config := reviewed
	model := filepath.Join(candidate, "examples", "backprop-tlc")
	for _, directory := range []string{config, model, scratch} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	data := map[string][]byte{}
	for _, name := range []string{"Spec.tla", "Spec.cfg", "positive.json"} {
		raw, err := os.ReadFile(filepath.Join(example, name))
		if err != nil {
			t.Fatal(err)
		}
		data[name] = raw
		destination := config
		if name == "positive.json" {
			destination = model
		}
		if err := os.WriteFile(filepath.Join(destination, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	java := filepath.Join("bin", "java")
	if runtime.GOOS == "windows" {
		java += ".exe"
	}
	manifest := executor.TLCManifest{
		Property: "finite-state-not-two", Invariant: "Safe", Specification: "Spec.tla", Configuration: "Spec.cfg",
		SpecDigest: journal.Digest(data["Spec.tla"]), ConfigDigest: journal.Digest(data["Spec.cfg"]),
		Jar: os.Getenv("GOOBERS_TEST_TLC_JAR"), Runtime: os.Getenv("GOOBERS_TEST_JAVA_HOME"),
		RuntimeDigest: tlcWorkflowRuntimeDigest(t), Java: java, Assumptions: []string{},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "manifest.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return reviewed, candidate, scratch
}

func tlcWorkflowRuntimeDigest(t *testing.T) string {
	t.Helper()
	type runtimeFile struct {
		Path   string `json:"path"`
		Digest string `json:"digest"`
	}
	var files []runtimeFile
	root := os.Getenv("GOOBERS_TEST_JAVA_HOME")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, runtimeFile{Path: filepath.ToSlash(relative), Digest: journal.Digest(raw)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	encoded, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return journal.Digest(encoded)
}

type tlcWorkflowWorkspace struct {
	path string
}

func (w tlcWorkflowWorkspace) Provision(_ context.Context, req engine.WorkspaceRequest) (engine.Workspace, error) {
	if req.Mode != apiv1.WorkspaceRepo {
		return nil, os.ErrInvalid
	}
	return w, nil
}

func (w tlcWorkflowWorkspace) Path() string                 { return w.path }
func (w tlcWorkflowWorkspace) Remove(context.Context) error { return os.RemoveAll(w.path) }
