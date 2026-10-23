package startpin

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
)

func loadReference(t *testing.T) (string, *instance.Config, *instance.ConfigSet) {
	t.Helper()
	root := filepath.Join("..", "..", "..", "reference-workflows")
	cfg, err := instance.LoadConfig(filepath.Join(root, "instance.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	set, report, err := instance.LoadConfigDir(root)
	if err != nil {
		t.Fatalf("load reference workflows: %v\n%+v", err, report)
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
	return root, cfg, set
}

func TestComputePinsEveryWorkflowAndMatchesTheWorkerDigest(t *testing.T) {
	root, cfg, set := loadReference(t)
	pins, err := Compute(root, cfg, set)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != len(set.Workflows) {
		t.Fatalf("pins = %d, want one per workflow (%d)", len(pins), len(set.Workflows))
	}
	// The worker resolves a pinned attempt through ConfigSnapshot.GooberDigestFor;
	// an admission digest that differs would refuse every attempt of every run.
	instructions, skills, err := runtimewiring.LoadSnapshotGooberInputs(root, set)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runtimewiring.ConfigSnapshot{Digest: "tree", Digests: runtimewiring.NewGooberDigestIndex(cfg, set, instructions, skills)}
	for i := range set.Workflows {
		w := &set.Workflows[i]
		got, ok := pins[w.Spec.Gaggle+"/"+w.Name]
		if !ok || got.WorkflowDigest == "" || got.GooberDigest == "" {
			t.Fatalf("%s/%s pins = %+v", w.Spec.Gaggle, w.Name, got)
		}
		want, err := snapshot.GooberDigestFor(w.Spec.Gaggle, w.Name)
		if err != nil || want != got.GooberDigest {
			t.Fatalf("%s/%s goober digest %q, worker computes %q (%v)", w.Spec.Gaggle, w.Name, got.GooberDigest, want, err)
		}
	}
	again, err := Compute(root, cfg, set)
	if err != nil {
		t.Fatal(err)
	}
	for key, pin := range pins {
		if again[key] != pin {
			t.Fatalf("%s pins are not deterministic", key)
		}
	}
}

func TestComputeRefusesWithoutLoadedConfiguration(t *testing.T) {
	if _, err := Compute(t.TempDir(), nil, nil); err == nil {
		t.Fatal("a missing configuration produced pins")
	}
}
