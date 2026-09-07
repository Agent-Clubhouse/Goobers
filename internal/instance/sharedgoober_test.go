package instance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigDirIncludesInstanceSharedGoober(t *testing.T) {
	configDir, path := sharedGooberFixture(t)
	set, report, err := LoadConfigDir(configDir)
	if err != nil {
		t.Fatalf("shared persona did not satisfy workflow reference: %v; report=%+v", err, report)
	}
	if len(set.Goobers) != 1 || set.Goobers[0].Name != "coder" || set.Goobers[0].Spec.Gaggle != "" {
		t.Fatalf("shared persona was dropped or assigned a fabricated gaggle: %+v", set.Goobers)
	}
	source, ok := set.GooberSource("coder")
	if !ok || filepath.Clean(filepath.Join(configDir, filepath.FromSlash(source))) != path {
		t.Fatalf("shared source provenance=%q, present=%v", source, ok)
	}
}

func sharedGooberFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.CopyFS(configDir, os.DirFS("starter")); err != nil {
		t.Fatal(err)
	}
	sharedRoot := filepath.Join(root, "goobers")
	if err := os.MkdirAll(sharedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(sharedRoot, "coder")
	if err := os.Rename(filepath.Join(configDir, "gaggles", "example", "goobers", "coder"), shared); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(shared, "goober.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), "  gaggle: example\n", "", 1)
	text = strings.Replace(text, "  workflows:\n    - default-implement\n", "", 1)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return configDir, path
}

func TestLoadConfigDirRejectsInvalidSharedGooberScope(t *testing.T) {
	for _, kind := range []string{"shared with owner", "local without owner", "collision", "wrong shared directory", "non-persona definition"} {
		t.Run(kind, func(t *testing.T) {
			configDir, path := sharedGooberFixture(t)
			local := filepath.Join(configDir, "gaggles", "example", "goobers", "coder")
			switch kind {
			case "shared with owner":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "spec:\n", "spec:\n  gaggle: example\n", 1)), 0o644); err != nil {
					t.Fatal(err)
				}
			case "local without owner":
				if err := os.Rename(filepath.Dir(path), local); err != nil {
					t.Fatal(err)
				}
			case "collision":
				if err := os.CopyFS(local, os.DirFS(filepath.Join("starter", "gaggles", "example", "goobers", "coder"))); err != nil {
					t.Fatal(err)
				}
			case "wrong shared directory":
				if err := os.Rename(filepath.Dir(path), filepath.Join(filepath.Dir(filepath.Dir(path)), "other")); err != nil {
					t.Fatal(err)
				}
			case "non-persona definition":
				raw, err := os.ReadFile(filepath.Join(configDir, "gaggles", "example", "gaggle.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "extra.yaml"), raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			set, report, err := LoadConfigDir(configDir)
			if !errors.Is(err, ErrInvalidConfig) || set != nil || report == nil || !report.HasErrors() {
				t.Fatalf("invalid scope admitted: set=%+v, report=%+v, err=%v", set, report, err)
			}
		})
	}
}
