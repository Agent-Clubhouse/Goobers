package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/interactive"
)

func configFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.CopyFS(path, os.DirFS("../../samples/interactive-config")); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDSLCatalogUsesNormalReferencesAndPinsSharedInstructions(t *testing.T) {
	source := configFixture(t)
	shared := filepath.Join(filepath.Dir(source), "goobers", "helper")
	if err := os.MkdirAll(filepath.Dir(shared), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(source, "gaggles", "interactive", "goobers", "helper"), shared); err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(shared, "goober.yaml")
	data, err := os.ReadFile(definition)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "  gaggle: interactive\n", "", 1))
	if err := os.WriteFile(definition, data, 0600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	catalog, err := loadDSLCatalog(source, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Surfaces) != 2 || catalog.Surfaces[0].Gaggle != "interactive" || catalog.Surfaces[0].Harness != "codex" || catalog.Surfaces[0].ConfigRevision != catalog.Revision {
		t.Fatalf("DSL bindings lost: %+v", catalog.Surfaces)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(catalog.ConfigDir), "goobers", "helper", "instructions.md")); err != nil {
		t.Fatal("shared instructions missing from immutable snapshot:", err)
	}
	instructions := filepath.Join(shared, "instructions.md")
	if err := os.WriteFile(instructions, []byte("Changed trusted helper instructions.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := loadDSLCatalog(source, state)
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision == catalog.Revision || next.Surfaces[0].Instructions == catalog.Surfaces[0].Instructions {
		t.Fatal("shared instruction edit did not change pinned policy")
	}
	stored, err := os.ReadFile(filepath.Join(filepath.Dir(catalog.ConfigDir), "goobers", "helper", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != catalog.Surfaces[0].Instructions {
		t.Fatal("prior config snapshot was mutated")
	}
}

func TestDSLCatalogRejectsLiveAuthorityAndInvalidReloadKeepsPolicy(t *testing.T) {
	source := configFixture(t)
	state := t.TempDir()
	catalog, err := loadDSLCatalog(source, state)
	if err != nil {
		t.Fatal(err)
	}
	service, err := interactive.Open(interactive.Options{StateDir: t.TempDir(), Principal: func(*http.Request) (string, error) { return "alice", nil }, Surfaces: catalog.Surfaces})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	store := catalogStore{current: catalog}
	gaggle := filepath.Join(source, "gaggles", "interactive", "gaggle.yaml")
	data, err := os.ReadFile(gaggle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gaggle, []byte("invalid: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.reload(source, state, service); err == nil {
		t.Fatal("invalid config reloaded")
	}
	if store.current.Revision != catalog.Revision {
		t.Fatal("invalid config replaced last good catalog")
	}
	data = []byte(strings.Replace(string(data), "brokerRef: controlled-local-ado", "brokerRef: live-ado", 1))
	if err := os.WriteFile(gaggle, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDSLCatalog(source, state); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("live binding was silently substituted: %v", err)
	}
}

func TestDSLCatalogRejectsUnsupportedGooberSettingsAndPreservesEmptyPolicy(t *testing.T) {
	source := configFixture(t)
	definition := filepath.Join(source, "gaggles", "interactive", "goobers", "helper", "goober.yaml")
	data, err := os.ReadFile(definition)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "spec:\n", "spec:\n  tools: [shell]\n", 1))
	if err := os.WriteFile(definition, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDSLCatalog(source, t.TempDir()); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported Goober restrictions silently dropped: %v", err)
	}
	source = configFixture(t)
	gaggle := filepath.Join(source, "gaggles", "interactive", "gaggle.yaml")
	data, err = os.ReadFile(gaggle)
	if err != nil {
		t.Fatal(err)
	}
	index := strings.Index(string(data), "  interactive:")
	if index < 0 {
		t.Fatal("fixture missing interactive block")
	}
	if err := os.WriteFile(gaggle, append(data[:index], []byte("  interactive: {}\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := loadDSLCatalog(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Surfaces) != 0 {
		t.Fatal("empty DSL block enabled implicit surfaces")
	}
}
