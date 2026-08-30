package dslmigrate_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	k8syaml "sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/workflow"
)

const referenceWorkflowsDir = "../../reference-workflows/gaggles/goobers/workflows"

func readReferenceWorkflows(t *testing.T) map[string]apiv1.Workflow {
	t.Helper()
	entries, err := os.ReadDir(referenceWorkflowsDir)
	if err != nil {
		t.Fatalf("read reference workflows: %v", err)
	}
	out := map[string]apiv1.Workflow{}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(referenceWorkflowsDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var wf apiv1.Workflow
		if err := k8syaml.Unmarshal(src, &wf); err != nil {
			t.Fatalf("%s: decode: %v", e.Name(), err)
		}
		if wf.Kind == "Workflow" {
			out[e.Name()] = wf
		}
	}
	return out
}

// TestAcceptanceEveryReferenceWorkflowMigratesClean is dsl-3.0.md §9 item 1's
// compile half: every shipped/reference workflow, run through
// `goobers fix --to 3.0`, validates clean under 3.0 with zero manual edits.
func TestAcceptanceEveryReferenceWorkflowMigratesClean(t *testing.T) {
	entries, err := os.ReadDir(referenceWorkflowsDir)
	if err != nil {
		t.Fatal(err)
	}
	migrated := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(referenceWorkflowsDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			res, err := dslmigrate.Migrate(src, "3.0")
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			var wf apiv1.Workflow
			if err := k8syaml.Unmarshal([]byte(res.After), &wf); err != nil {
				t.Fatalf("decode migrated: %v\n%s", err, res.After)
			}
			if wf.DSLVersion != "3.0" {
				t.Fatalf("migrated dslVersion = %q, want 3.0", wf.DSLVersion)
			}
			if _, err := workflow.Compile(workflow.Definition{
				Name: wf.Name, DSLVersion: wf.DSLVersion, Spec: wf.Spec,
			}, workflow.WithPreviewFeatures(true)); err != nil {
				t.Fatalf("migrated workflow does not validate clean under 3.0: %v\n%s", err, res.After)
			}
			migrated++
		})
	}
	if migrated == 0 {
		t.Fatal("no reference workflows were migrated")
	}
}

// TestAcceptanceCommitReadingEdgeTable is the frozen migrator comparison
// fixture (dsl-3.0.md §9 item 10, Goobernetes-Delivery decisions 001/002): the
// repoFrom edges the migrator derives for the reference gaggle, recomputed
// under the COMMIT reading (definitions = branch-ref-advancing stages only).
// The bindings below are the discriminators the decision record froze; a
// `[implement]`-only local-ci is the back-edge-pruning failure and an
// automatic fail. Comparison is set-normalized.
func TestAcceptanceCommitReadingEdgeTable(t *testing.T) {
	want := map[string]map[string][]string{
		// The discriminator: the CI-repass lane creates true fan-in at
		// local-ci; back-edge pruning would drop remediate-ci and yield
		// [implement] alone. implement is itself re-entered on this shape
		// (review needs-changes → implement, local-gate fail → implement), so
		// it carries the remediate-ci back-edge; that is correct
		// reaching-definitions, not a bug.
		"implementation.yaml": {
			"implement":    {"remediate-ci"},
			"remediate-ci": {"implement"},
			"local-ci":     {"implement", "remediate-ci"},
			"push-branch":  {"implement", "remediate-ci"},
			"open-pr":      {"implement", "remediate-ci"},
		},
		// The zero-repo-stage negative: merge-review has no repo-producing
		// stage, so no stage receives a repoFrom edge.
		"merge-review.yaml": {},
		// Empty-set fixtures: the sole producer is the first repo stage of the
		// run, so it has no incoming edge; its downstream consumers cover it.
		"docs-updater.yaml": {
			"update-docs": nil, // producer, empty set — receives no repoFrom
			"validate":    {"update-docs"},
			"push-branch": {"update-docs"},
			"open-pr":     {"update-docs"},
		},
		"tutor.yaml": {
			"analyze":      nil, // producer, empty set
			"draft-change": {"analyze"},
		},
	}

	workflows := readReferenceWorkflows(t)
	for file, expected := range want {
		t.Run(file, func(t *testing.T) {
			wf, ok := workflows[file]
			if !ok {
				t.Fatalf("reference workflow %s not found", file)
			}
			got := migratedRepoFrom(t, wf)
			for stage, wantEdges := range expected {
				gotEdges := got[stage]
				if !equalSet(gotEdges, wantEdges) {
					t.Fatalf("%s stage %q repoFrom = %v, want %v (set-normalized)", file, stage, gotEdges, wantEdges)
				}
			}
			// merge-review: assert NO stage carries a repoFrom edge.
			if len(expected) == 0 {
				for stage, edges := range got {
					if len(edges) > 0 {
						t.Fatalf("%s: zero-repo-stage negative violated — stage %q got repoFrom %v", file, stage, edges)
					}
				}
			}
		})
	}
}

// migratedRepoFrom migrates wf to 3.0 and returns the repoFrom map the migrator
// inserted, keyed by stage name.
func migratedRepoFrom(t *testing.T, wf apiv1.Workflow) map[string][]string {
	t.Helper()
	src, err := k8syaml.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	res, err := dslmigrate.Migrate(src, "3.0")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var migrated apiv1.Workflow
	if err := k8syaml.Unmarshal([]byte(res.After), &migrated); err != nil {
		t.Fatalf("decode migrated: %v", err)
	}
	out := map[string][]string{}
	for _, task := range migrated.Spec.Tasks {
		out[task.Name] = append([]string(nil), []string(task.RepoFrom)...)
	}
	return out
}

// TestAcceptanceConformanceEquivalentTransitionGraph is the journal-conformance
// proxy for dsl-3.0.md §9 item 1's second half: on a single runner, repoFrom
// and runsOn are inert (§4 "byte-identical behavior" in modes 1/2), so the
// migrated 3.0 machine must present the exact same stage-transition graph as
// the 2.0 original — the graph a conformance journal records. A full runtime
// journal diff on the local runner is the recorded follow-up.
func TestAcceptanceConformanceEquivalentTransitionGraph(t *testing.T) {
	entries, err := os.ReadDir(referenceWorkflowsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(referenceWorkflowsDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var original apiv1.Workflow
			if err := k8syaml.Unmarshal(src, &original); err != nil {
				t.Fatal(err)
			}
			if original.Kind != "Workflow" {
				return
			}
			m20, err := workflow.Compile(workflow.Definition{
				Name: original.Name, DSLVersion: original.DSLVersion, Spec: original.Spec,
			})
			if err != nil {
				t.Fatalf("compile 2.0 original: %v", err)
			}

			res, err := dslmigrate.Migrate(src, "3.0")
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			var migrated apiv1.Workflow
			if err := k8syaml.Unmarshal([]byte(res.After), &migrated); err != nil {
				t.Fatal(err)
			}
			m30, err := workflow.Compile(workflow.Definition{
				Name: migrated.Name, DSLVersion: migrated.DSLVersion, Spec: migrated.Spec,
			}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatalf("compile 3.0 migrated: %v", err)
			}

			for _, state := range stateNames(original) {
				before := sortedSet(m20.Outgoing(state))
				after := sortedSet(m30.Outgoing(state))
				if strings.Join(before, ",") != strings.Join(after, ",") {
					t.Fatalf("state %q transitions changed by migration: 2.0=%v 3.0=%v", state, before, after)
				}
			}
		})
	}
}

func stateNames(wf apiv1.Workflow) []string {
	var names []string
	for _, t := range wf.Spec.Tasks {
		names = append(names, t.Name)
	}
	for _, g := range wf.Spec.Gates {
		names = append(names, g.Name)
	}
	for _, p := range wf.Spec.Parallels {
		names = append(names, p.Name)
	}
	return names
}

func sortedSet(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalSet(a, b []string) bool {
	as := sortedSet(a)
	bs := sortedSet(b)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
