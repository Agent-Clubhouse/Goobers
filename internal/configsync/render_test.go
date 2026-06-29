package configsync

import (
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/v1alpha1"
)

func TestWriteManifests(t *testing.T) {
	out := t.TempDir()
	set := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web"), managedGaggle("api")}}
	written, err := set.WriteManifests(out)
	if err != nil {
		t.Fatalf("WriteManifests: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("wrote %d files, want 2", len(written))
	}

	// Each rendered file round-trips to a valid CR with the stamped namespace.
	data, err := os.ReadFile(filepath.Join(out, "gaggle-web.yaml"))
	if err != nil {
		t.Fatalf("read rendered: %v", err)
	}
	var g v1alpha1.Gaggle
	if err := yaml.Unmarshal(data, &g); err != nil {
		t.Fatalf("rendered manifest not valid YAML CR: %v", err)
	}
	if g.Namespace != DefaultNamespace || g.Kind != "Gaggle" {
		t.Errorf("rendered CR not stamped: ns=%q kind=%q", g.Namespace, g.Kind)
	}
}

// TestWriteManifests_PrunesStale verifies the render directory reflects exactly
// the current desired state: a previously-rendered object dropped from the set
// has its file removed, so ArgoCD prunes it from the cluster.
func TestWriteManifests_PrunesStale(t *testing.T) {
	out := t.TempDir()
	full := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web"), managedGaggle("api")}}
	if _, err := full.WriteManifests(out); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "gaggle-api.yaml")); err != nil {
		t.Fatalf("expected gaggle-api.yaml after first render: %v", err)
	}

	// Second render drops "api".
	reduced := &RenderSet{Namespace: DefaultNamespace, Objects: []client.Object{managedGaggle("web")}}
	if _, err := reduced.WriteManifests(out); err != nil {
		t.Fatalf("second render: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "gaggle-api.yaml")); !os.IsNotExist(err) {
		t.Errorf("stale gaggle-api.yaml should be pruned from render dir, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "gaggle-web.yaml")); err != nil {
		t.Errorf("gaggle-web.yaml should remain: %v", err)
	}
}

func TestSortObjects_DeterministicKindOrder(t *testing.T) {
	mk := func(kind, name string) client.Object {
		o := &v1alpha1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: name}}
		o.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(kind))
		return o
	}
	objs := []client.Object{mk("Workflow", "w"), mk("Manifest", "m"), mk("Goober", "g"), mk("Gaggle", "a")}
	sortObjects(objs)
	want := []string{"Manifest", "Gaggle", "Goober", "Workflow"}
	for i, k := range want {
		if got := objs[i].GetObjectKind().GroupVersionKind().Kind; got != k {
			t.Errorf("position %d = %s, want %s", i, got, k)
		}
	}
}
