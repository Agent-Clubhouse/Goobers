package workflow

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestScheduledResweepWorkflowsCompileWithIndependentBudgets(t *testing.T) {
	for _, tc := range []struct{ root, goober string }{
		{"config-examples/gaggles/acme-web", "curator"},
		{"config-examples/gaggles/acme-web-claude", "claude-curator"},
		{"reference-workflows/gaggles/goobers", "curator"},
	} {
		t.Run(tc.root, func(t *testing.T) {
			root := filepath.Join("..", "..", filepath.FromSlash(tc.root))
			var w apiv1.Workflow
			readResweepFixture(t, filepath.Join(root, "workflows", "curate-resweep.yaml"), &w)
			var g apiv1.Goober
			readResweepFixture(t, filepath.Join(root, "goobers", tc.goober, "goober.yaml"), &g)
			def := Definition{Name: w.Name, Version: 1, Spec: w.Spec}
			m, err := compileAcknowledged(def, WithGoobers(map[string]apiv1.GooberSpec{g.Name: g.Spec}))
			if err != nil {
				t.Fatal(err)
			}
			if warnings := CheckWarnings(def); len(warnings) != 0 {
				t.Fatalf("warnings: %v", warnings)
			}
			if len(w.Spec.Triggers) != 1 || w.Spec.Triggers[0].Type != apiv1.TriggerSchedule || w.Spec.Triggers[0].Schedule != "47 4 * * *" {
				t.Fatalf("missing independent daily schedule: %+v", w.Spec.Triggers)
			}
			if w.Spec.Readiness.MaxConcurrentRuns != 1 || w.Spec.Readiness.MaxRunsPerHour != 1 || w.Spec.Readiness.MaxRunsPerDay != 1 {
				t.Fatalf("missing readiness bounds: %+v", w.Spec.Readiness)
			}
			query, ok := m.Task("query-resweep")
			if !ok || query.Run == nil || !slices.Equal(query.Run.Command, []string{"goobers", "backlog-query", "--claim", "--resweep"}) {
				t.Fatalf("not an explicit claiming sweep: %+v", query)
			}
			if query.Inputs["resweepMaxItems"] != "5" || query.Inputs["maxItems"] != "20" || query.Inputs["resweepInterval"] != "" || query.Inputs["resultFile"] != "claimed-items.json" {
				t.Fatalf("selection limits/output or cadence ownership drifted: %+v", query.Inputs)
			}
			for _, edge := range [][2]string{{"query-resweep", "surface-duplicates"}, {"surface-duplicates", "curate"}, {"curate", "release-claim"}, {"release-claim", ""}} {
				task, ok := m.Task(edge[0])
				if !ok || task.Next != edge[1] {
					t.Fatalf("broken handoff %v", edge)
				}
			}
			var ordinary apiv1.Workflow
			readResweepFixture(t, filepath.Join(root, "workflows", "backlog-curation.yaml"), &ordinary)
			for _, task := range ordinary.Spec.Tasks {
				if task.Inputs["resweepMaxItems"] != "" {
					t.Fatal("ordinary curation still enables inline sweep")
				}
			}
		})
	}
}

func readResweepFixture(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
