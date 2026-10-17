package instance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInteractiveLoadConfigDirPreservesPoliciesAndSourceBindings(t *testing.T) {
	set, report, err := LoadConfigDir("../../samples/interactive-config")
	if err != nil {
		t.Fatalf("load: %v %+v", err, report)
	}
	if len(set.Gaggles) != 1 || len(set.Goobers) != 2 || len(set.Workflows) != 1 {
		t.Fatalf("unexpected loaded set: %+v", set)
	}
	g := set.Gaggles[0]
	if g.DSLVersion != "3.0" || g.Spec.Interactive == nil || len(g.Spec.Interactive.Surfaces) != 2 {
		t.Fatalf("lost interactive pin/policy: %+v", g)
	}
	s := g.Spec.Interactive.Surfaces[0]
	if s.GooberRef != "helper" || s.DefaultExecutionMode != "codex-local" || s.Session.MaxChildren != 3 || len(s.AuthorityRefs) != 2 {
		t.Fatalf("lost binding: %+v", s)
	}
	if p, ok := set.GooberSource(s.GooberRef); !ok || !strings.HasSuffix(p, "helper/goober.yaml") {
		t.Fatalf("source binding %q %v", p, ok)
	}
	copy := g.DeepCopy()
	copy.Spec.Interactive.Surfaces[0].Tools[0] = "changed"
	copy.Spec.Interactive.Surfaces[0].ChildWorkflows.CommandCapabilities[0] = "changed"
	copy.Spec.Interactive.Authorities[0].BrokerRef = "changed"
	if g.Spec.Interactive.Surfaces[0].Tools[0] == "changed" || g.Spec.Interactive.Surfaces[0].ChildWorkflows.CommandCapabilities[0] == "changed" || g.Spec.Interactive.Authorities[0].BrokerRef == "changed" {
		t.Fatal("deep copy aliases policy")
	}
}

func TestInteractiveInvalidReferenceFailsClosedAtNormalLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../samples/interactive-config")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gaggles/interactive/gaggle.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(b), "gooberRef: helper", "gooberRef: absent")), 0600); err != nil {
		t.Fatal(err)
	}
	set, report, err := LoadConfigDir(dir)
	if !errors.Is(err, ErrInvalidConfig) || set != nil || report == nil || !report.HasErrors() {
		t.Fatalf("load did not fail closed: set=%v report=%v err=%v", set, report, err)
	}
}

func TestInteractiveEmptyBlockLoadsWithoutImplicitSurface(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../samples/interactive-config")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gaggles/interactive/gaggle.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	at := strings.Index(s, "  interactive:")
	if at < 0 {
		t.Fatal("missing interactive block")
	}
	if err := os.WriteFile(path, []byte(s[:at]+"  interactive: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	set, report, err := LoadConfigDir(dir)
	if err != nil {
		t.Fatalf("empty optional config rejected: %v %+v", err, report)
	}
	if len(set.Gaggles[0].Spec.Interactive.Surfaces) != 0 {
		t.Fatal("loader synthesized an unauthorized surface")
	}
}
