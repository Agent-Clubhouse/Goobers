package cloudreadiness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestCloudReadinessReportsEveryBlockerClass(t *testing.T) {
	result := Assess(&instance.Config{}, blockerFixture())
	if result.CloudReady {
		t.Fatal("result is cloud-ready with blockers")
	}
	if result.Summary.BlockedWorkflows != 2 {
		t.Fatalf("blocked workflows = %d, want 2", result.Summary.BlockedWorkflows)
	}
	codes := map[string]bool{}
	features := map[string]bool{}
	for _, workflow := range result.Workflows {
		for _, blocker := range workflow.Blockers {
			codes[blocker.Code] = true
			features[blocker.Feature] = true
			if blocker.Path == "" || blocker.Path[0] != '/' {
				t.Errorf("%s/%s blocker has invalid path %q", workflow.Gaggle, workflow.Name, blocker.Path)
			}
			if blocker.Code == CodeFeature && (blocker.Action.Issue == 0 || blocker.Action.IssueURL == "") {
				t.Errorf("%s has no P4 issue action: %+v", blocker.Feature, blocker.Action)
			}
			if blocker.Code == CodePlacement && blocker.Stage == "" {
				t.Errorf("placement blocker has no stage: %+v", blocker)
			}
		}
	}
	for _, code := range []string{CodeFeature, CodePlacement, CodeDSL} {
		if !codes[code] {
			t.Errorf("missing blocker class %s", code)
		}
	}
	for _, feature := range []string{
		"spec.parallels", "spec.parallels.selection", "spec.maps",
		"task.childWorkflows", "gate.evaluator.human",
		"task.limits.maxTokens", "task.limits.maxCostUSD",
		"task.experiment",
	} {
		if !features[feature] {
			t.Errorf("missing engine feature blocker %s", feature)
		}
	}
	if result.Summary.ConfigFixes == 0 || result.Summary.WaitingOnP4 == 0 ||
		len(result.Summary.P4Issues) != 7 {
		t.Fatalf("summary = %+v, want config and all 8 P4 issue groups", result.Summary)
	}
}

func TestCloudReadinessJSONStable(t *testing.T) {
	result := Assess(&instance.Config{}, blockerFixture())
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	assertGolden(t, filepath.Join("testdata", "cloud-readiness.golden.json"), data)
}

func TestReferenceGaggleCloudReadinessGolden(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows")
	cfg, err := instance.LoadConfig(filepath.Join(root, "instance.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	set, report, err := instance.LoadConfigDir(root)
	if err != nil {
		t.Fatalf("load reference workflows: %v\n%+v", err, report)
	}
	result := Assess(cfg, set)
	counts := map[string]int{}
	featureWorkflows := map[string]bool{}
	for _, workflow := range result.Workflows {
		for _, blocker := range workflow.Blockers {
			counts[blocker.Code]++
			if blocker.Code == CodeFeature {
				featureWorkflows[workflow.Name] = true
			}
		}
	}
	if counts[CodeDSL] != 16 {
		t.Errorf("RUNTIME003 count = %d, want 16", counts[CodeDSL])
	}
	wantFeatureWorkflows := map[string]bool{"quality-sprint": true, "quality-spot-check": true}
	if !reflect.DeepEqual(featureWorkflows, wantFeatureWorkflows) {
		t.Errorf("RUNTIME001 workflows = %v, want %v", featureWorkflows, wantFeatureWorkflows)
	}
	var text bytes.Buffer
	if err := WriteText(&text, result); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, filepath.Join("testdata", "reference-gaggle.golden.txt"), text.Bytes())
}

func blockerFixture() *instance.ConfigSet {
	deterministic := apiv1.Task{
		Name: "local", Type: apiv1.TaskDeterministic, Goal: "run locally",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}},
	}
	agentic := apiv1.Task{
		Name: "agent", Type: apiv1.TaskAgentic, Goal: "agent work", Goober: "coder",
		ChildWorkflows: &apiv1.ChildWorkflowPolicy{},
		Experiment:     &apiv1.BanditExperiment{},
		Outbox:         []string{"result.json"},
		Limits:         &apiv1.Limits{MaxTokens: 100, MaxCostUSD: 1},
	}
	return &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{
			ObjectMeta: metav1.ObjectMeta{Name: "g"},
		}},
		Workflows: []apiv1.Workflow{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "legacy"},
				DSLVersion: supportmatrix.V2DSLVersion,
				Spec: apiv1.WorkflowSpec{
					Gaggle: "g", Start: "local", Tasks: []apiv1.Task{deterministic},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "all-features"},
				DSLVersion: supportmatrix.V31DSLVersion,
				Spec: apiv1.WorkflowSpec{
					Gaggle: "g", Start: "fan", Tasks: []apiv1.Task{agentic},
					Parallels: []apiv1.Parallel{{
						Name: "fan", Selection: &apiv1.ParallelSelection{},
					}},
					Maps: []apiv1.Map{{Name: "map"}},
					Gates: []apiv1.Gate{
						{Name: "approval", Evaluator: apiv1.EvaluatorHuman},
						{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "coder"}},
					},
				},
			},
		},
		Goobers: []apiv1.Goober{{
			ObjectMeta: metav1.ObjectMeta{Name: "coder"},
			Spec:       apiv1.GooberSpec{Gaggle: "g", Harness: apiv1.HarnessCopilot},
		}},
	}
}

func assertGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs; regenerate with UPDATE_GOLDEN=1 go test ./internal/cloudreadiness", path)
	}
}

func TestWriteTextCloudReady(t *testing.T) {
	var output strings.Builder
	if err := WriteText(&output, Assess(&instance.Config{}, &instance.ConfigSet{})); err != nil {
		t.Fatal(err)
	}
	if output.String() != "cloud-ready\n" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestUnplaceableWorkflowFailsClosed(t *testing.T) {
	set := blockerFixture()
	set.Workflows = set.Workflows[:1]
	set.Workflows[0].DSLVersion = supportmatrix.V3DSLVersion
	set.Workflows[0].Spec.Tasks[0].RunsOn = &apiv1.RunsOn{Capabilities: []string{"no-such-capability"}}
	cfg := &instance.Config{
		Placement: &instance.PlacementConfig{SelfExecution: "deny"},
		Runners: []instance.RunnerEntry{{
			Name: "worker", Host: "ghcr.io/example/worker:v1",
			Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux, Shell: true},
		}},
	}
	result := Assess(cfg, set)
	if result.CloudReady || len(result.Workflows) != 1 || result.Workflows[0].Blockers[0].Code != CodePlacement {
		t.Fatalf("unplaceable workflow assessed ready: %+v", result)
	}
}
