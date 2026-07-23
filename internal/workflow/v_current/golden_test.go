package vcurrent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestGoldenCompiledSemanticDigests(t *testing.T) {
	goldenDir := filepath.Join("testdata", "golden")
	raw, err := os.ReadFile(filepath.Join(goldenDir, "digests.json"))
	if err != nil {
		t.Fatalf("read golden digests: %v", err)
	}
	var digests map[string]goldenDigests
	if err := json.Unmarshal(raw, &digests); err != nil {
		t.Fatalf("decode golden digests: %v", err)
	}
	fixtures, err := filepath.Glob(filepath.Join(goldenDir, "*.yaml"))
	if err != nil {
		t.Fatalf("list golden fixtures: %v", err)
	}
	if len(fixtures) != len(digests) {
		t.Fatalf("golden fixture count = %d, digest count = %d", len(fixtures), len(digests))
	}
	for _, fixture := range fixtures {
		if _, ok := digests[filepath.Base(fixture)]; !ok {
			t.Fatalf("fixture %q has no frozen digest", filepath.Base(fixture))
		}
	}

	for file, want := range digests {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(goldenDir, file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var parsed apiv1.Workflow
			if err := yaml.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			machine, err := Compile(Definition{
				Name:       parsed.Name,
				Version:    1,
				DSLVersion: parsed.DSLVersion,
				Spec:       parsed.Spec,
			}, WithPreviewFeatures(true))
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if got := machine.Digest(); got != want.Machine {
				t.Fatalf("compiled machine digest drift:\n got  %s\n want %s", got, want.Machine)
			}
			got, err := semanticDigest(machine)
			if err != nil {
				t.Fatalf("semanticDigest: %v", err)
			}
			if got != want.Semantics {
				t.Fatalf("compiled semantic digest drift:\n got  %s\n want %s", got, want.Semantics)
			}
		})
	}
}

type goldenDigests struct {
	Machine   string `json:"machine"`
	Semantics string `json:"semantics"`
}

type compiledSemantics struct {
	DSLVersion       string              `json:"dslVersion"`
	DefinitionDigest string              `json:"definitionDigest"`
	Start            string              `json:"start"`
	States           []compiledState     `json:"states"`
	Features         []Feature           `json:"features"`
	Validation       validationSemantics `json:"validation"`
}

type compiledState struct {
	Name             string            `json:"name"`
	Kind             string            `json:"kind"`
	Outgoing         []string          `json:"outgoing"`
	Task             *apiv1.Task       `json:"task,omitempty"`
	Gate             *apiv1.Gate       `json:"gate,omitempty"`
	InvocationInputs map[string]string `json:"invocationInputs,omitempty"`
	Limits           apiv1.Limits      `json:"limits"`
}

type validationSemantics struct {
	Warnings               []string            `json:"warnings,omitempty"`
	Reachability           []string            `json:"reachability,omitempty"`
	Schedules              []string            `json:"schedules,omitempty"`
	TriggerFields          []string            `json:"triggerFields,omitempty"`
	Admission              []string            `json:"admission,omitempty"`
	GateParameters         []string            `json:"gateParameters,omitempty"`
	GateOutcomes           []string            `json:"gateOutcomes,omitempty"`
	StageRequiredInputs    []string            `json:"stageRequiredInputs,omitempty"`
	StageContracts         []string            `json:"stageContracts,omitempty"`
	StageContractWarnings  []string            `json:"stageContractWarnings,omitempty"`
	TimeoutCoherence       []string            `json:"timeoutCoherence,omitempty"`
	FeaturesWithoutPreview []FeatureDiagnostic `json:"featuresWithoutPreview,omitempty"`
	FeaturesWithPreview    []FeatureDiagnostic `json:"featuresWithPreview,omitempty"`
}

func semanticDigest(machine *Machine) (string, error) {
	if machine == nil {
		return "", fmt.Errorf("workflow machine is nil")
	}

	features, err := FeaturesForWorkflow(machine.Def)
	if err != nil {
		return "", fmt.Errorf("resolve workflow features: %w", err)
	}

	semantics := compiledSemantics{
		DSLVersion:       machine.Def.DSLVersion,
		DefinitionDigest: machine.Digest(),
		Start:            machine.Def.Spec.Start,
		Features:         features,
		Validation: validationSemantics{
			Warnings:               CheckWarnings(machine.Def),
			Reachability:           CheckReachability(machine.Def),
			Schedules:              CheckSchedules(machine.Def),
			TriggerFields:          CheckTriggerFields(machine.Def),
			Admission:              CheckWorkflowAdmission(machine.Def, nil),
			GateParameters:         CheckGateParameters(machine.Def),
			GateOutcomes:           CheckGateOutcomes(machine.Def),
			StageRequiredInputs:    CheckStageRequiredInputs(machine.Def),
			StageContracts:         CheckStageContracts(machine.Def),
			StageContractWarnings:  CheckStageContractWarnings(machine.Def),
			TimeoutCoherence:       CheckStageTimeoutCoherence(machine.Def),
			FeaturesWithoutPreview: CheckWorkflowFeatureSupport(machine.Def, false),
			FeaturesWithPreview:    CheckWorkflowFeatureSupport(machine.Def, true),
		},
	}

	names := make([]string, 0, len(machine.Def.Spec.Tasks)+len(machine.Def.Spec.Gates))
	for _, task := range machine.Def.Spec.Tasks {
		names = append(names, task.Name)
	}
	for _, gate := range machine.Def.Spec.Gates {
		names = append(names, gate.Name)
	}
	sort.Strings(names)

	for _, name := range names {
		state := compiledState{Name: name, Outgoing: machine.Outgoing(name)}
		if task, ok := machine.Task(name); ok {
			state.Kind = "task"
			state.Task = &task
			state.InvocationInputs = TaskInvocationInputs(machine, task)
			state.Limits = TaskLimits(task)
		} else if gate, ok := machine.Gate(name); ok {
			state.Kind = "gate"
			state.Gate = &gate
			state.Limits = GateLimits(gate)
		} else {
			return "", fmt.Errorf("compiled state %q is missing from machine lookup", name)
		}
		semantics.States = append(semantics.States, state)
	}

	raw, err := json.Marshal(semantics)
	if err != nil {
		return "", fmt.Errorf("marshal compiled semantics: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
