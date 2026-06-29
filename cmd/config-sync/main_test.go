package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/configsync"
)

func devnull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func writeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must := func(p, content string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("manifest.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata: {name: inst}
spec:
  instance: {name: acme, environment: dev}
  gaggles: [web]
`)
	must("gaggles/web/gaggle.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata: {name: web}
spec:
  project: {provider: github, owner: acme, name: web}
  backlog: {provider: github, project: acme/web}
  isolation: {namespace: gaggle-web}
`)
	return dir
}

func TestRun_RenderMode(t *testing.T) {
	cfg := writeRepo(t)
	out := t.TempDir()
	code := run([]string{"--config", cfg, "--out", out}, devnull(t), devnull(t))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(out, "gaggle-web.yaml")); err != nil {
		t.Errorf("expected rendered gaggle-web.yaml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "manifest-inst.yaml")); err != nil {
		t.Errorf("expected rendered manifest-inst.yaml: %v", err)
	}
}

// TestRun_RenderIdempotentNestedOut reproduces QA-2's bug: rendering into a
// directory nested under the config repo must stay idempotent — the second
// render must not re-ingest the first render's output.
func TestRun_RenderIdempotentNestedOut(t *testing.T) {
	cfg := writeRepo(t)
	out := filepath.Join(cfg, "rendered") // nested under the config root
	for i := 0; i < 2; i++ {
		if code := run([]string{"--config", cfg, "--out", out}, devnull(t), devnull(t)); code != 0 {
			t.Fatalf("render %d: exit = %d, want 0", i+1, code)
		}
	}
	// Output is the desired set, not doubled.
	if _, err := os.Stat(filepath.Join(out, "gaggle-web.yaml")); err != nil {
		t.Errorf("expected gaggle-web.yaml after idempotent renders: %v", err)
	}
}

func TestRun_InvalidConfigExitsOne(t *testing.T) {
	dir := t.TempDir()
	// Missing required fields => validation errors.
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("apiVersion: goobers.dev/v1alpha1\nkind: Manifest\nmetadata: {name: x}\nspec: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := run([]string{"--config", dir, "--out", t.TempDir()}, devnull(t), devnull(t))
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for invalid config", code)
	}
}

func TestRun_BadFlagExitsTwo(t *testing.T) {
	if code := run([]string{"--nope"}, devnull(t), devnull(t)); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestRun_ApplyMode(t *testing.T) {
	cfg := writeRepo(t)
	orig := applyFn
	t.Cleanup(func() { applyFn = orig })

	var appliedObjs int
	applyFn = func(_ context.Context, set *configsync.RenderSet) error {
		appliedObjs = len(set.Objects)
		return nil
	}
	if code := run([]string{"--config", cfg, "--apply"}, devnull(t), devnull(t)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if appliedObjs == 0 {
		t.Error("apply path did not receive the render set")
	}
}

func TestRun_ApplyFailureExitsOne(t *testing.T) {
	cfg := writeRepo(t)
	orig := applyFn
	t.Cleanup(func() { applyFn = orig })
	applyFn = func(context.Context, *configsync.RenderSet) error { return errBoom }
	if code := run([]string{"--config", cfg, "--apply"}, devnull(t), devnull(t)); code != 1 {
		t.Fatalf("exit = %d, want 1 on apply failure", code)
	}
}

var errBoom = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }
