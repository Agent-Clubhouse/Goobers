package startpin

import (
	"path/filepath"
	"strings"
	"testing"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
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
	// Deliberately NOT overlaid: Compute must apply the overlays itself.
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
	assertWorkflowDigestsMatchAdmission(t, set, pins)
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

// assertWorkflowDigestsMatchAdmission compares every pin's workflow digest with
// the machine the admission path compiles (daemon, child admission): the digest
// a worker later resolves.
func assertWorkflowDigestsMatchAdmission(t *testing.T, set *instance.ConfigSet, pins map[string]cloudconfig.Pins) {
	t.Helper()
	var harnesses []string
	for i := range set.Goobers {
		harnesses = append(harnesses, string(set.Goobers[i].Spec.Harness))
	}
	machines, err := runtimewiring.CompileWorkflowMachines(set, runtimewiring.GoobersByName(set), harnesses, nil)
	if err != nil {
		t.Fatal(err)
	}
	for identity, machine := range machines {
		if got := pins[identity.Gaggle+"/"+identity.Workflow].WorkflowDigest; got != machine.Digest() {
			t.Errorf("%s/%s workflow digest %q, admission compiles %q", identity.Gaggle, identity.Workflow, got, machine.Digest())
		}
	}
}

func TestComputeAppliesGaggleOverlaysItself(t *testing.T) {
	root, cfg, raw := loadReference(t)
	_, _, overlaid := loadReference(t)
	for i := range raw.Gaggles {
		raw.Gaggles[i].Spec.CICommand = []string{"make", "verify"}
		overlaid.Gaggles[i].Spec.CICommand = []string{"make", "verify"}
	}
	instance.ApplyGaggleCICommand(overlaid)
	instance.ApplyGaggleOutboxMirror(overlaid)
	want, err := Compute(root, cfg, overlaid)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Compute(root, cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("pins = %d, want %d", len(got), len(want))
	}
	for key, pin := range want {
		if got[key] != pin {
			t.Errorf("%s: pins from the raw set %+v differ from the overlaid set's %+v", key, got[key], pin)
		}
	}
	// With a CICommand the pins must also equal what a worker resolves.
	instructions, skills, err := runtimewiring.LoadSnapshotGooberInputs(root, overlaid)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runtimewiring.ConfigSnapshot{Digest: "tree", Digests: runtimewiring.NewGooberDigestIndex(cfg, overlaid, instructions, skills)}
	for key, pin := range got {
		gaggle, name, _ := strings.Cut(key, "/")
		if goober, err := snapshot.GooberDigestFor(gaggle, name); err != nil || goober != pin.GooberDigest {
			t.Errorf("%s goober digest %q, worker computes %q (%v)", key, pin.GooberDigest, goober, err)
		}
	}
	assertWorkflowDigestsMatchAdmission(t, overlaid, got)
}
