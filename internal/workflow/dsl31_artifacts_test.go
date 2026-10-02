package workflow

import (
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestDSL31ArtifactContractsCompileAndResolveFeatures(t *testing.T) {
	def := Definition{
		Name:       "artifact-contract",
		DSLVersion: supportmatrix.V31DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "_produce",
			Tasks: []apiv1.Task{
				{
					Name: "_produce", Type: apiv1.TaskDeterministic, Goal: "produce",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactSlots: []apiv1.ArtifactSlot{
						{Name: "_report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json", MaxSize: 1024},
						{Name: "trace", MediaType: "application/x-ndjson"},
					},
					Next: "consume",
				},
				{
					Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume",
					Run:            &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactInputs: map[string]apiv1.ArtifactInputRef{"_summary": {From: "_produce._report"}, "telemetry": {From: "_produce.trace"}},
				},
			},
		},
	}
	if _, err := Compile(def, WithPreviewFeatures(true)); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	machine, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile for binding check: %v", err)
	}
	bindings := machine.ArtifactBindings("consume")
	if got := bindings["_summary"]; got.ProducerTask != "_produce" || got.SlotName != "_report" ||
		got.MediaType != "application/json" || got.SchemaPath != "schemas/report.schema.json" || got.MaxSize != 1024 {
		t.Fatalf("semantic artifact binding _summary = %+v", got)
	}
	if got := bindings["telemetry"]; got.ProducerTask != "_produce" || got.SlotName != "trace" || got.MediaType != "application/x-ndjson" {
		t.Fatalf("semantic artifact binding telemetry = %+v", got)
	}
	features, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	got := featureIDStrings(features)
	for _, want := range []string{
		"task.artifactSlots",
		"task.artifactSlots.mediaType",
		"task.artifactSlots.schemaPath",
		"task.artifactSlots.maxSize",
		"task.artifactInputs",
		"task.artifactInputs.from",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("features = %v, want %s", got, want)
		}
	}
}

func TestDSL30RejectsArtifactContractSurface(t *testing.T) {
	baseTask := apiv1.Task{
		Name: "produce", Type: apiv1.TaskDeterministic, Goal: "produce",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
	}
	for _, tc := range []struct {
		name      string
		mutate    func(*apiv1.Task)
		wantError string
	}{
		{
			name:      "non-empty artifactSlots",
			mutate:    func(task *apiv1.Task) { task.ArtifactSlots = []apiv1.ArtifactSlot{{Name: "report"}} },
			wantError: `artifactSlots, which requires dslVersion "3.1"`,
		},
		{
			name:      "empty artifactSlots",
			mutate:    func(task *apiv1.Task) { task.ArtifactSlots = []apiv1.ArtifactSlot{} },
			wantError: `artifactSlots, which requires dslVersion "3.1"`,
		},
		{
			name:      "empty artifactInputs",
			mutate:    func(task *apiv1.Task) { task.ArtifactInputs = map[string]apiv1.ArtifactInputRef{} },
			wantError: `artifactInputs, which requires dslVersion "3.1"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := baseTask
			tc.mutate(&task)
			def := Definition{
				Name:       "artifact-contract",
				DSLVersion: supportmatrix.V3DSLVersion,
				Spec: apiv1.WorkflowSpec{
					Gaggle:   "web",
					Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
					Start:    "produce",
					Tasks:    []apiv1.Task{task},
				},
			}
			_, err := Compile(def, WithPreviewFeatures(true))
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Compile error = %v, want DSL 3.1 surface refusal containing %q", err, tc.wantError)
			}
		})
	}
}

func TestDSL31RejectsUnknownArtifactInputSlot(t *testing.T) {
	def := Definition{
		Name:       "artifact-contract",
		DSLVersion: supportmatrix.V31DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "produce",
			Tasks: []apiv1.Task{
				{
					Name: "produce", Type: apiv1.TaskDeterministic, Goal: "produce",
					Run:           &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactSlots: []apiv1.ArtifactSlot{{Name: "report"}},
					Next:          "consume",
				},
				{
					Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume",
					Run:            &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactInputs: map[string]apiv1.ArtifactInputRef{"summary": {From: "produce.missing"}},
				},
			},
		},
	}
	_, err := Compile(def, WithPreviewFeatures(true))
	if err == nil || !strings.Contains(err.Error(), `artifact input "summary" references unknown artifact slot "missing" on producer "produce"`) {
		t.Fatalf("Compile error = %v, want unknown slot refusal", err)
	}
}

func TestDSL31RejectsArtifactBindingFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*Definition)
		wantError string
	}{
		{
			name: "duplicate producer slot",
			mutate: func(def *Definition) {
				def.Spec.Tasks[0].ArtifactSlots = append(def.Spec.Tasks[0].ArtifactSlots, apiv1.ArtifactSlot{Name: "report"})
			},
			wantError: `artifactSlots repeats slot "report"`,
		},
		{
			name: "invalid local consumer name",
			mutate: func(def *Definition) {
				def.Spec.Tasks[1].ArtifactInputs["bad.name"] = apiv1.ArtifactInputRef{From: "produce.report"}
			},
			wantError: `artifactInputs contains invalid local input name "bad.name"`,
		},
		{
			name: "malformed reference",
			mutate: func(def *Definition) {
				def.Spec.Tasks[1].ArtifactInputs["summary"] = apiv1.ArtifactInputRef{From: "produce.report.extra"}
			},
			wantError: `artifact input "summary" must reference a producer slot as producer.slot`,
		},
		{
			name: "unknown producer",
			mutate: func(def *Definition) {
				def.Spec.Tasks[1].ArtifactInputs["summary"] = apiv1.ArtifactInputRef{From: "missing.report"}
			},
			wantError: `artifact input "summary" references unknown producer task "missing"`,
		},
		{
			name: "unknown slot",
			mutate: func(def *Definition) {
				def.Spec.Tasks[1].ArtifactInputs["summary"] = apiv1.ArtifactInputRef{From: "produce.missing"}
			},
			wantError: `artifact input "summary" references unknown artifact slot "missing" on producer "produce"`,
		},
		{
			name: "self reference",
			mutate: func(def *Definition) {
				def.Spec.Tasks[1].ArtifactSlots = []apiv1.ArtifactSlot{{Name: "own"}}
				def.Spec.Tasks[1].ArtifactInputs["own"] = apiv1.ArtifactInputRef{From: "consume.own"}
			},
			wantError: `artifact input "own" references itself`,
		},
		{
			name: "producer not on every path",
			mutate: func(def *Definition) {
				*def = artifactConditionalProducerWorkflow()
			},
			wantError: `producer "produce", but "produce" does not run on every successful path before "consume"`,
		},
		{
			name: "media type mismatch",
			mutate: func(def *Definition) {
				def.Spec.Tasks[1].ArtifactInputs["summary"] = apiv1.ArtifactInputRef{From: "produce.report", MediaType: "text/plain"}
			},
			wantError: `expects mediaType "text/plain", but producer "produce" slot "report" declares "application/json"`,
		},
		{
			name: "schema mismatch",
			mutate: func(def *Definition) {
				def.Spec.Tasks[1].ArtifactInputs["summary"] = apiv1.ArtifactInputRef{From: "produce.report", SchemaPath: "schemas/other.schema.json"}
			},
			wantError: `expects schemaPath "schemas/other.schema.json", but producer "produce" slot "report" declares "schemas/report.schema.json"`,
		},
		{
			name: "consumer media type without producer declaration",
			mutate: func(def *Definition) {
				def.Spec.Tasks[0].ArtifactSlots[0].MediaType = ""
			},
			wantError: `expects mediaType "application/json", but producer "produce" slot "report" declares no mediaType`,
		},
		{
			name: "consumer schema without producer declaration",
			mutate: func(def *Definition) {
				def.Spec.Tasks[0].ArtifactSlots[0].SchemaPath = ""
			},
			wantError: `expects schemaPath "schemas/report.schema.json", but producer "produce" slot "report" declares no schemaPath`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := artifactLinearWorkflow()
			tc.mutate(&def)
			_, err := Compile(def, WithPreviewFeatures(true))
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Compile error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestDSL31ParallelBranchArtifactBindingAllowsSameBranchConsumer(t *testing.T) {
	def := Definition{
		Name:       "artifact-branch-consumer",
		DSLVersion: supportmatrix.V31DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fanout",
			Parallels: []apiv1.Parallel{{
				Name:          "fanout",
				Join:          "join",
				FailurePolicy: apiv1.BranchContinueOnError,
				Branches: []apiv1.Branch{
					{Name: "left", Start: "left-produce"},
					{Name: "right", Start: "right-produce"},
				},
			}},
			Tasks: []apiv1.Task{
				artifactProducerTask("left-produce", "left-slot", "left-consume"),
				{
					Name: "left-consume", Type: apiv1.TaskDeterministic, Goal: "consume",
					Run:            &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactInputs: map[string]apiv1.ArtifactInputRef{"left": {From: "left-produce.left-slot"}},
					Next:           TargetJoin,
				},
				artifactProducerTask("right-produce", "right-slot", TargetJoin),
				{
					Name: "join", Type: apiv1.TaskDeterministic, Goal: "join",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				},
			},
		},
	}
	machine, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	bindings := machine.ArtifactBindings("left-consume")
	if got := bindings["left"]; got.ProducerTask != "left-produce" || got.SlotName != "left-slot" {
		t.Fatalf("left-consume binding = %+v", got)
	}
}

func TestDSL31ParallelFanInArtifactBindingsUseDeclaredProducerSlots(t *testing.T) {
	def := Definition{
		Name:       "artifact-parallel-fanin",
		DSLVersion: supportmatrix.V31DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fanout",
			Parallels: []apiv1.Parallel{{
				Name:          "fanout",
				Join:          "join",
				FailurePolicy: apiv1.BranchContinueOnError,
				Branches: []apiv1.Branch{
					{Name: "left", Start: "left-produce"},
					{Name: "right", Start: "right-produce"},
				},
			}},
			Tasks: []apiv1.Task{
				artifactProducerTask("left-produce", "left-slot", TargetJoin),
				artifactProducerTask("right-produce", "right-slot", TargetJoin),
				{
					Name: "join", Type: apiv1.TaskDeterministic, Goal: "join",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactInputs: map[string]apiv1.ArtifactInputRef{
						"left":  {From: "left-produce.left-slot"},
						"right": {From: "right-produce.right-slot"},
					},
				},
			},
		},
	}
	machine, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	bindings := machine.ArtifactBindings("join")
	if bindings["left"].ProducerTask != "left-produce" || bindings["left"].SlotName != "left-slot" {
		t.Fatalf("left binding = %+v", bindings["left"])
	}
	if bindings["right"].ProducerTask != "right-produce" || bindings["right"].SlotName != "right-slot" {
		t.Fatalf("right binding = %+v", bindings["right"])
	}
}

func TestDSL31ParallelFanInRejectsBranchProducerSkippedOnSuccessfulPath(t *testing.T) {
	def := artifactParallelSkippedProducerWorkflow()
	_, err := Compile(def, WithPreviewFeatures(true))
	if err == nil || !strings.Contains(err.Error(), `producer "left-produce", but "left-produce" does not run on every successful path before "join"`) {
		t.Fatalf("Compile error = %v, want skipped branch producer refusal", err)
	}
}

func artifactLinearWorkflow() Definition {
	return Definition{
		Name:       "artifact-contract",
		DSLVersion: supportmatrix.V31DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "produce",
			Tasks: []apiv1.Task{
				artifactProducerTask("produce", "report", "consume"),
				{
					Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume",
					Run:            &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactInputs: map[string]apiv1.ArtifactInputRef{"summary": {From: "produce.report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"}},
				},
			},
		},
	}
}

func artifactProducerTask(name, slot, next string) apiv1.Task {
	return apiv1.Task{
		Name: name, Type: apiv1.TaskDeterministic, Goal: "produce",
		Run:           &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		ArtifactSlots: []apiv1.ArtifactSlot{{Name: slot, MediaType: "application/json", SchemaPath: "schemas/report.schema.json", MaxSize: 2048}},
		Next:          next,
	}
}

func artifactConditionalProducerWorkflow() Definition {
	def := artifactLinearWorkflow()
	def.Spec.Start = "choose"
	def.Spec.Gates = []apiv1.Gate{{
		Name:      "choose",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "ci-status"},
		Branches:  map[string]string{"pass": "produce", "fail": "consume", "timeout": "consume"},
	}}
	return def
}

func artifactParallelSkippedProducerWorkflow() Definition {
	return Definition{
		Name:       "artifact-parallel-skipped",
		DSLVersion: supportmatrix.V31DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fanout",
			Parallels: []apiv1.Parallel{{
				Name:          "fanout",
				Join:          "join",
				FailurePolicy: apiv1.BranchContinueOnError,
				Branches: []apiv1.Branch{
					{Name: "left", Start: "left-choice"},
					{Name: "right", Start: "right-produce"},
				},
			}},
			Gates: []apiv1.Gate{{
				Name:      "left-choice",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "ci-status"},
				Branches:  map[string]string{"pass": "left-produce", "fail": TargetJoin, "timeout": TargetJoin},
			}},
			Tasks: []apiv1.Task{
				artifactProducerTask("left-produce", "left-slot", TargetJoin),
				artifactProducerTask("right-produce", "right-slot", TargetJoin),
				{
					Name: "join", Type: apiv1.TaskDeterministic, Goal: "join",
					Run:            &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactInputs: map[string]apiv1.ArtifactInputRef{"left": {From: "left-produce.left-slot"}},
				},
			},
		},
	}
}

func featureIDStrings(features []Feature) []string {
	out := make([]string, len(features))
	for i, feature := range features {
		out[i] = string(feature.ID)
	}
	return out
}
