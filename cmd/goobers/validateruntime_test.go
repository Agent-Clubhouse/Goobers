package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

func TestRuntimeSupportColumnMatchesEngineRefusals(t *testing.T) {
	var got []workflow.FeatureID
	for _, feature := range workflow.AllFeatures() {
		if runtimeLevel(feature, workflow.RuntimeEngine) == workflow.RuntimeUnsupported {
			got = append(got, feature.ID)
		}

		if runtimeLevel(feature, workflow.RuntimeRunner) != workflow.RuntimeSupported {
			t.Errorf("%s runner support = %q, want supported", feature.ID, runtimeLevel(feature, workflow.RuntimeRunner))
		}
	}
	var want []workflow.FeatureID
	for id := range engineRuntimeBlockers {
		want = append(want, id)
	}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("engine-unsupported registry features = %v, want refusal set %v", got, want)
	}

	spec := apiv1.WorkflowSpec{
		Parallels: []apiv1.Parallel{{Selection: &apiv1.ParallelSelection{}}},
		Maps:      []apiv1.Map{{Name: "map"}},
		Tasks: []apiv1.Task{{
			Name: "agent", Type: apiv1.TaskAgentic,
			ChildWorkflows: &apiv1.ChildWorkflowPolicy{},
			Experiment:     &apiv1.BanditExperiment{},
			Outbox:         []string{"result.json"},
			Limits:         &apiv1.Limits{MaxTokens: 1, MaxCostUSD: 1},
		}},
		Gates: []apiv1.Gate{{Name: "approval", Evaluator: apiv1.EvaluatorHuman}},
	}
	declared := declaredEngineRuntimeBlockers(spec)
	if !reflect.DeepEqual(declared, want) {
		t.Fatalf("engine refusal fixture maps to %v, want registry unsupported set %v", declared, want)
	}
}

func TestValidateTargetCloudReportsRuntimeBlockers(t *testing.T) {
	root := initIntrospectionInstance(t)

	localCode, localOut, localErr := runArgs(t, "validate", root)
	if localCode != 0 || localErr != "" {
		t.Fatalf("local validate = code %d, stdout %q, stderr %q", localCode, localOut, localErr)
	}
	if strings.Contains(localOut, "RUNTIME002") || strings.Contains(localOut, "RUNTIME003") {
		t.Fatalf("default local validation reported cloud-only blockers:\n%s", localOut)
	}

	cloudCode, cloudOut, cloudErr := runArgs(t, "validate", "--target", "cloud", root)
	if cloudCode != 1 || cloudErr != "" {
		t.Fatalf("cloud validate = code %d, stdout %q, stderr %q", cloudCode, cloudOut, cloudErr)
	}
	for _, code := range []string{"RUNTIME002", "RUNTIME003"} {
		if !strings.Contains(cloudOut, code) {
			t.Errorf("cloud validate omitted %s:\n%s", code, cloudOut)
		}
	}
	if !strings.Contains(cloudOut, "goobers fix --to 3.0") {
		t.Errorf("cloud validate omitted DSL migration guidance:\n%s", cloudOut)
	}

	if code, _, stderr := runArgs(t, "validate", "--target", "edge", root); code != 2 ||
		!strings.Contains(stderr, "--target must be local or cloud") {
		t.Fatalf("invalid target = code %d, stderr %q", code, stderr)
	}
}

func TestRuntimeFindingSeverities(t *testing.T) {
	parallel := apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "parallel"},
		DSLVersion: supportmatrix.V31DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "g",
			Parallels: []apiv1.Parallel{{
				Name: "fan-out",
			}},
		},
	}
	task := apiv1.Task{
		Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}},
	}
	selfPinned := apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "self-pinned"},
		DSLVersion: supportmatrix.V3DSLVersion,
		Spec:       apiv1.WorkflowSpec{Gaggle: "g", Tasks: []apiv1.Task{task}},
	}
	legacy := apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy"},
		DSLVersion: supportmatrix.V2DSLVersion,
		Spec:       apiv1.WorkflowSpec{Gaggle: "g", Tasks: []apiv1.Task{task}},
	}

	tests := []struct {
		name   string
		cfg    *instance.Config
		target validateTarget
		wf     apiv1.Workflow
		want   map[validate.WarningCode]validate.Severity
	}{
		{
			name: "local feature information", cfg: &instance.Config{},
			target: validateTargetLocal, wf: parallel,
			want: map[validate.WarningCode]validate.Severity{validate.RuntimeEngineUnsupported: validate.Info},
		},
		{
			name: "engine feature warning", cfg: &instance.Config{Engine: &instance.EngineConfig{}},
			target: validateTargetLocal, wf: parallel,
			want: map[validate.WarningCode]validate.Severity{validate.RuntimeEngineUnsupported: validate.Warning},
		},
		{
			name: "cloud feature error", cfg: &instance.Config{},
			target: validateTargetCloud, wf: parallel,
			want: map[validate.WarningCode]validate.Severity{validate.RuntimeEngineUnsupported: validate.Error},
		},
		{
			name: "engine self placement warning", cfg: &instance.Config{Engine: &instance.EngineConfig{}},
			target: validateTargetLocal, wf: selfPinned,
			want: map[validate.WarningCode]validate.Severity{validate.RuntimeSelfExecutionRequired: validate.Warning},
		},
		{
			name: "cloud legacy blockers", cfg: &instance.Config{},
			target: validateTargetCloud, wf: legacy,
			want: map[validate.WarningCode]validate.Severity{
				validate.RuntimeSelfExecutionRequired: validate.Error,
				validate.RuntimeDSLVersionUnsupported: validate.Error,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := &instance.ConfigSet{
				Gaggles:   []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "g"}}},
				Workflows: []apiv1.Workflow{tc.wf},
			}
			findings := appendRuntimeFindings(".", "config", tc.cfg, set, tc.target, &validate.Report{})
			got := make(map[validate.WarningCode]validate.Severity, len(findings))
			for _, finding := range findings {
				got[finding.code] = finding.severity
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("findings = %v, want %v", got, tc.want)
			}
		})
	}
}
