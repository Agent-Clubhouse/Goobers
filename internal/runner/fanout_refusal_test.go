package runner

import (
	"context"
	"errors"
	"os"
	"testing"

	apischemas "github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/workflow"
)

func fanOutTestTask(name, next string) apiv1.Task {
	return apiv1.Task{Name: name, Type: apiv1.TaskDeterministic, Goal: name,
		Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: next}
}

func fanOutSourceTask(name, next string) apiv1.Task {
	task := fanOutTestTask(name, next)
	task.ArtifactSlots = []apiv1.ArtifactSlot{{Name: "plan", MediaType: "application/json"}}
	return task
}

func compileFanOutMachine(t *testing.T, spec apiv1.WorkflowSpec) *workflow.Machine {
	t.Helper()
	spec.Gaggle, spec.Triggers = "web", []apiv1.Trigger{{Type: apiv1.TriggerManual}}
	m, err := workflow.Compile(workflow.Definition{Name: "fanout", Version: 1, DSLVersion: "3.1", Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func selectiveParallelMachine(t *testing.T) *workflow.Machine {
	return compileFanOutMachine(t, apiv1.WorkflowSpec{
		Start: "triage",
		Tasks: []apiv1.Task{fanOutSourceTask("triage", "review"), fanOutTestTask("baseline", workflow.TargetJoin), fanOutTestTask("security", workflow.TargetJoin), fanOutTestTask("collate", "")},
		Parallels: []apiv1.Parallel{{
			Name: "review", FailurePolicy: apiv1.BranchContinueOnError, Join: "collate", MaxConcurrentBranches: 1,
			Selection: &apiv1.ParallelSelection{Source: apiv1.FanOutSource{Task: "triage", Slot: "plan"}},
			Branches:  []apiv1.Branch{{Name: "baseline", Start: "baseline"}, {Name: "security", Start: "security", Optional: true}},
		}},
	})
}

func mapMachine(t *testing.T) *workflow.Machine {
	return compileFanOutMachine(t, apiv1.WorkflowSpec{
		Start: "plan",
		Tasks: []apiv1.Task{fanOutSourceTask("plan", "items"), fanOutTestTask("item", workflow.TargetJoin), fanOutTestTask("collate", "")},
		Maps: []apiv1.Map{{Name: "items", Source: apiv1.FanOutSource{Task: "plan", Slot: "plan"}, Task: "item",
			FailurePolicy: apiv1.BranchContinueOnError, Join: "collate"}},
	})
}

// TestFanOutRefusedBeforeLocalEffects pins design §13: until FAN-2 and FAN-4
// land, the local runner refuses selective parallels and maps at every entry
// point with a typed error, before touching run storage, and never runs a
// selective parallel as a plain parallel.
func TestFanOutRefusedBeforeLocalEffects(t *testing.T) {
	for _, construct := range []struct {
		name     string
		machine  *workflow.Machine
		sentinel error
	}{
		{"selective parallel", selectiveParallelMachine(t), workflow.ErrSelectiveParallelUnsupported},
		{"map", mapMachine(t), workflow.ErrMapUnsupported},
	} {
		m := construct.machine
		runs := t.TempDir()
		r := &Runner{cfg: Config{RunsDir: runs}}
		for _, entry := range []struct {
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
			t.Run(construct.name+"/"+entry.name, func(t *testing.T) {
				_, err := entry.call()
				var typed *workflow.UnsupportedFanOutError
				if !errors.Is(err, construct.sentinel) || !errors.As(err, &typed) {
					t.Fatalf("runtime guard = %v, want typed %v", err, construct.sentinel)
				}
				entries, err := os.ReadDir(runs)
				if err != nil || len(entries) != 0 {
					t.Fatalf("refused execution touched run storage: %v %v", entries, err)
				}
			})
		}
	}
}

// TestFanOutSourceSlotEffectiveSchemaIsEnvelope pins design §4.1 through the
// producer schema loader path: a fan-out source slot without schemaPath is
// held to the shipped envelope at publish_output, and the configuration
// loader is never asked for it.
func TestFanOutSourceSlotEffectiveSchemaIsEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		machine  *workflow.Machine
		producer string
		envelope string
		valid    string
		invalid  string
	}{
		{"selection", selectiveParallelMachine(t), "triage", apischemas.FanOutSelection,
			`{"schemaVersion":1,"branches":[{"name":"security","reason":"touches auth"}]}`, `{"branches":["security"]}`},
		{"roster", mapMachine(t), "plan", apischemas.FanOutRoster,
			`{"schemaVersion":1,"items":[{"identity":"unit-a","item":{"packages":["./a/..."]}}]}`, `{"schemaVersion":1,"items":[{"identity":"-bad","item":{}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, _ := tc.machine.Task(tc.producer)
			load := func(path string) (*handoffcheck.Schema, error) {
				t.Fatalf("configuration loader asked for %q; the envelope is host-owned", path)
				return nil, nil
			}
			got, err := producerPublicationSchemas(tc.machine, task, load)
			if err != nil {
				t.Fatal(err)
			}
			schema := got["plan"]
			if schema == nil || schema.ID() != tc.envelope {
				t.Fatalf("effective schemas = %v, want slot plan bound to %s", got, tc.envelope)
			}
			if verdict := schema.Check([]byte(tc.valid)); !verdict.Valid {
				t.Fatalf("valid document rejected: %+v", verdict.Issues)
			}
			if verdict := schema.Check([]byte(tc.invalid)); verdict.Valid {
				t.Fatalf("invalid document accepted by %s", tc.envelope)
			}
			if disabled, err := producerPublicationSchemas(tc.machine, task, nil); err != nil || disabled != nil {
				t.Fatalf("nil loader must keep publication validation disabled: %v %v", disabled, err)
			}
		})
	}
}
