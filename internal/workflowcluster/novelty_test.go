package workflowcluster

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func asWorkflow(def workflow.Definition) apiv1.Workflow {
	var wf apiv1.Workflow
	wf.Name, wf.DSLVersion, wf.Spec, wf.Annotations = def.Name, def.DSLVersion, def.Spec, def.Annotations
	return wf
}

func loopShape(i int) workflow.Definition {
	loop := reviewLoop([4]string{"implement", "review", "verdict", "publish"}, strings.Repeat("x", i))
	loop.Spec.Tasks[0].Retry.MaxAttempts = int32(2 + i%2)
	return loop
}

func savedLoopModel(t *testing.T) (Model, string) {
	t.Helper()
	var points []Point
	for i := range 6 {
		points = append(points, StructuralPoint(fmt.Sprintf("loop-%d", i), extract(t, loopShape(i))))
	}
	model, err := Fit(StructuralFeatureVersion, points, Options{MinClusterSize: 3, Resamples: 20, Seed: 1, AllowSingleCluster: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Clusters) != 1 {
		t.Fatalf("clusters = %+v, want one loop cluster", model.Clusters)
	}
	raw, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "model.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return model, path
}

func TestLoadModelVerifiesContentAddress(t *testing.T) {
	model, path := savedLoopModel(t)
	loaded, err := LoadModel(path)
	if err != nil || loaded.ID != model.ID {
		t.Fatalf("LoadModel = %s, %v; want %s", loaded.ID, err, model.ID)
	}

	raw, _ := os.ReadFile(path)
	tampered := strings.Replace(string(raw), `"stable": true`, `"stable": false`, 1)
	if tampered == string(raw) {
		tampered = strings.Replace(string(raw), `"population": 6`, `"population": 7`, 1)
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadModel(path); err == nil {
		t.Fatal("LoadModel accepted a model whose content no longer matches its id")
	}
	if _, err := LoadModel(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("LoadModel accepted a missing file")
	}
}

func TestScoreWorkflowSeparatesFamiliarAndNovelShapes(t *testing.T) {
	model, _ := savedLoopModel(t)

	// Renamed nodes with a member's sizes: names never contribute to features.
	renamed := reviewLoop([4]string{"build", "inspect", "decide", "ship"}, strings.Repeat("z", 3))
	familiar := ScoreWorkflow(&model, asWorkflow(renamed))
	if !familiar.Scored || familiar.OutOfDistribution || familiar.ClusterID != "c0" || familiar.ModelID != model.ID || familiar.ModelVersion != ModelVersion || familiar.NearestSize != 6 {
		t.Fatalf("familiar = %+v", familiar)
	}
	if !strings.HasPrefix(familiar.Summary(), "similar to cluster c0") {
		t.Fatalf("familiar summary = %q", familiar.Summary())
	}

	linear := reviewLoop([4]string{"a", "b", "c", "d"}, "")
	linear.Spec.Gates[0].Branches = map[string]string{"approved": "d", "changes": ""}
	linear.Spec.Tasks[0].Retry = nil
	linear.Spec.Tasks[2].Capabilities = nil
	novel := ScoreWorkflow(&model, asWorkflow(linear))
	if !novel.Scored || !novel.OutOfDistribution || novel.ClusterID != "" || novel.Nearest != "c0" || novel.Distance <= familiar.Distance {
		t.Fatalf("novel = %+v (familiar distance %v)", novel, familiar.Distance)
	}
	if !strings.HasPrefix(novel.Summary(), "novel shape") {
		t.Fatalf("novel summary = %q", novel.Summary())
	}
	if again := ScoreWorkflow(&model, asWorkflow(linear)); again != novel {
		t.Fatalf("score is not deterministic: %+v vs %+v", again, novel)
	}
}

func TestScoreWorkflowDegradesToUnscored(t *testing.T) {
	model, _ := savedLoopModel(t)
	model.FeatureVersion = "workflowcluster.structural/v0"
	stale := ScoreWorkflow(&model, asWorkflow(loopShape(1)))
	if stale.Scored || stale.Reason != UnscoredStaleModel || stale.Summary() != "unscored (stale_model)" {
		t.Fatalf("stale = %+v", stale)
	}
	if missing := ScoreWorkflow(nil, asWorkflow(loopShape(1))); missing.Scored || missing.Reason != UnscoredInvalidModel {
		t.Fatalf("nil model = %+v", missing)
	}
}
