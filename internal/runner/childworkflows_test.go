package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func childEnabledMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	m, err := workflow.Compile(workflow.Definition{Name: "children", Version: 1, DSLVersion: "3.1", Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "implement",
		Tasks: []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goal: "delegate", Goober: "coder", ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}},
	}}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestChildWorkflowExecutionRefusedBeforeLocalEffects(t *testing.T) {
	m := childEnabledMachine(t)
	runs := t.TempDir()
	r := &Runner{cfg: Config{RunsDir: runs}}
	for _, tc := range []struct {
		name string
		call func() (Result, error)
	}{
		{"start", func() (Result, error) { return r.Start(context.Background(), StartInput{RunID: "blocked", Machine: m}) }},
		{"resume", func() (Result, error) {
			return r.Resume(context.Background(), ResumeInput{RunID: "blocked", Machine: m})
		}},
		{"terminal resume", func() (Result, error) {
			return r.ResumeFromTerminal(context.Background(), ResumeFromTerminalInput{RunID: "blocked", Machine: m})
		}},
		{"rerun", func() (Result, error) {
			return r.RerunStage(context.Background(), RerunStageInput{RunID: "blocked", Machine: m})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.call(); !errors.Is(err, workflow.ErrChildWorkflowExecutionUnsupported) {
				t.Fatalf("runtime guard: %v", err)
			}
			entries, err := os.ReadDir(runs)
			if err != nil || len(entries) != 0 {
				t.Fatalf("refused execution touched run storage: %v %v", entries, err)
			}
		})
	}
}

func TestChildWorkflowResumeReadsPinWithoutMutation(t *testing.T) {
	m := childEnabledMachine(t)
	runs := t.TempDir()
	newPinnedDefinitionRun(t, runs, "pinned", m)
	dir := filepath.Join(runs, "pinned")
	before := snapshotChildRunFiles(t, dir)
	r := &Runner{cfg: Config{RunsDir: runs}}
	if _, err := r.Resume(context.Background(), ResumeInput{RunID: "pinned"}); !errors.Is(err, workflow.ErrChildWorkflowExecutionUnsupported) {
		t.Fatalf("pinned resume guard: %v", err)
	}
	if after := snapshotChildRunFiles(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatal("refused resume changed journal files")
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Events(); err != nil {
		t.Fatalf("journal no longer readable: %v", err)
	}
}

func snapshotChildRunFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
