package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
)

func mixedCostGaggles(t *testing.T, root string) (string, string) {
	t.Helper()
	layout := instance.NewLayout(root)
	set, _, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Gaggles) != 1 {
		t.Fatal("expected single demo gaggle")
	}
	origin := set.Gaggles[0].DeepCopy()
	source, ok := set.GaggleSource(origin.Name)
	if !ok {
		t.Fatal("missing source")
	}
	on, off := true, false
	origin.Spec.Cost = &apiv1.CostReporting{Enabled: &on}
	write := func(path string, value any) {
		t.Helper()
		if gaggle, ok := value.(*apiv1.Gaggle); ok {
			// Configuration documents have no reconciler-owned status field.
			value = map[string]any{"apiVersion": gaggle.APIVersion, "kind": gaggle.Kind, "metadata": gaggle.ObjectMeta, "spec": gaggle.Spec}
		}
		raw, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(layout.ConfigDir(), source), origin)
	other := origin.DeepCopy()
	other.Name = "cost-disabled"
	other.Spec.Isolation.Namespace = "cost-disabled"
	other.Spec.Cost.Enabled = &off
	write(filepath.Join(layout.ConfigDir(), "gaggles", other.Name, "gaggle.yaml"), other)
	set.Manifest.Spec.Gaggles = append(set.Manifest.Spec.Gaggles, other.Name)
	write(filepath.Join(layout.ConfigDir(), "manifest.yaml"), set.Manifest)
	if _, report, err := instance.LoadConfigDir(layout.ConfigDir()); err != nil {
		t.Fatalf("mixed fixture invalid: %v %+v", err, report)
	}
	return origin.Name, other.Name
}

func TestCostPublicationLegacyMixedGagglesRefusesGuessing(t *testing.T) {
	root := initDemo(t)
	on, off := mixedCostGaggles(t, root)
	for _, tc := range []struct {
		name string
		want bool
	}{{on, true}, {off, false}, {"", false}} {
		t.Setenv(executor.GaggleEnvVar, on)
		got, err := resolveCostPublication(root, tc.name, postMergeTestRepo())
		if err != nil || got != tc.want {
			t.Fatalf("gaggle=%q enabled=%v err=%v want=%v", tc.name, got, err, tc.want)
		}
	}
}

func TestCostPublicationDelayedUsesOriginNotSweepGaggle(t *testing.T) {
	t.Run("origin-enabled", func(t *testing.T) { testDelayedCostOrigin(t, true) })
	t.Run("origin-disabled", func(t *testing.T) { testDelayedCostOrigin(t, false) })
}

func testDelayedCostOrigin(t *testing.T, enabled bool) {
	t.Helper()
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, nil)
	st.prComments = []string{costComment(t, "goobers", "implementation", "cost-run", 20, 8_000_000_000).Body}
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root := postMergeReconcileEnv(t, server.URL)
	origin, sweep := mixedCostGaggles(t, root)
	if !enabled {
		origin, sweep = sweep, origin
	}
	t.Setenv(executor.GaggleEnvVar, origin)
	if err := recordPostMergeTimeout(root, postMergeTestRepo(), "20", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(executor.GaggleEnvVar, sweep)
	code, stdout, stderr := runArgs(t, "reconcile-post-merge", root)
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	count := 0
	for _, comment := range st.prComments {
		if strings.Contains(comment, postMergeCostSummaryMarker) {
			count++
		}
	}
	want := 0
	if enabled {
		want = 1
	}
	if count != want || st.issueState[42] != "closed" {
		t.Fatalf("summaries=%d issue=%s stderr=%s", count, st.issueState[42], stderr)
	}
}
