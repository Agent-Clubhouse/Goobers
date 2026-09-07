package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceTreeDefinitionDirPreservesFlatAndNestedLayouts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		flat, nested bool
		wantNested   bool
	}{
		{name: "absent"},
		{name: "flat", flat: true},
		{name: "nested", nested: true, wantNested: true},
		{name: "root manifest remains authoritative", flat: true, nested: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.flat {
				writeFileContent(t, filepath.Join(root, "manifest.yaml"), "invalid root manifest must not be bypassed")
			}
			if tc.nested {
				if err := os.Mkdir(filepath.Join(root, "config"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFileContent(t, filepath.Join(root, "config", "manifest.yaml"), "nested")
			}
			want := root
			if tc.wantNested {
				want = filepath.Join(root, "config")
			}
			if got := sourceTreeDefinitionDir(root); got != want {
				t.Fatalf("selected %q, want %q", got, want)
			}
		})
	}
}
