package configsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/v1alpha1"
)

func TestLoadSharedGooberWithAndWithoutSourceStaging(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "staged"}[staged], func(t *testing.T) {
			root := t.TempDir()
			config := filepath.Join(root, "config")
			if err := os.CopyFS(config, os.DirFS("../instance/starter")); err != nil {
				t.Fatal(err)
			}
			shared := filepath.Join(root, "goobers", "coder")
			if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(config, "gaggles", "example", "goobers", "coder"), shared); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(shared, "goober.yaml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "  gaggle: example\n", "", 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			var ignored []string
			if staged {
				output := filepath.Join(config, "rendered")
				if err := os.Mkdir(output, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(output, "bad.yaml"), []byte("not a definition"), 0o644); err != nil {
					t.Fatal(err)
				}
				ignored = []string{output}
			}
			loader, err := NewLoader("")
			if err != nil {
				t.Fatal(err)
			}
			set, report, err := loader.Load(config, ignored...)
			if err != nil {
				t.Fatalf("shared source failed: %v; report=%+v", err, report)
			}
			goobers := objectsByKind(set.Objects)["Goober"]
			if len(goobers) != 1 {
				t.Fatalf("rendered shared personas: %+v", goobers)
			}
			goober := goobers[0].(*v1alpha1.Goober)
			if goober.Name != "coder" || goober.Spec.Gaggle != "" || goober.Labels[GaggleLabel] != "" {
				t.Fatalf("shared persona acquired a fabricated owner: %+v", goober)
			}
		})
	}
}
