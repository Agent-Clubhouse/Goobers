package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/configsignal"
	"github.com/goobers/goobers/internal/instance"
)

func TestConfigPollSignalTracksAllDigestInputs(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "config")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "goober.yaml"), "kind: Goober\nspec:\n  instructions: ../external/body.md\n  skills: [shared]\n")
	write(filepath.Join(home, "external", "body.md"), "instructions")
	write(filepath.Join(home, "skills", "shared", "SKILL.md"), "skill")
	write(filepath.Join(root, "goobers", "coder", "assets", "file.txt"), "asset")
	var cache configsignal.Cache
	now := time.Now()
	calls := 0
	digest := func() string {
		t.Helper()
		result, err := cache.Digest(now, []string{root, filepath.Join(home, "goobers"), filepath.Join(home, "skills")}, func(observe func(string)) (string, error) {
			calls++
			return configDirectoryDigestObserved(root, "", observe)
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	previous := digest()
	for i := 0; i < 10; i++ {
		if got := digest(); got != previous {
			t.Fatal("unchanged digest moved")
		}
	}
	if calls != 1 {
		t.Fatalf("unchanged polls hashed %d times", calls)
	}
	for _, edit := range []struct{ path, body string }{
		{"config/goober.yaml", "kind: Goober\nspec:\n  instructions: ../external/body.md\n  skills: [shared]\n# changed definition\n"},
		{"external/body.md", "changed external instructions"},
		{"skills/shared/SKILL.md", "changed skill body"},
		{"skills/shared/support/new.md", "new nested skill support file"},
		{"config/goobers/coder/assets/file.txt", "changed asset"},
		{"config/goobers/coder/assets/nested/new.txt", "new nested asset"},
		{"goobers/shared/goober.yaml", "kind: Goober\nspec: {}\n"},
		{"config/gaggles/example/workflow.yaml", "kind: Workflow\nspec: {}\n"},
	} {
		write(filepath.Join(home, edit.path), edit.body)
		next := digest()
		if next == previous {
			t.Fatalf("missed %s", edit.path)
		}
		previous = next
	}
	for _, path := range []string{"external/body.md", "skills/shared/support/new.md", "config/goobers/coder/assets/nested/new.txt"} {
		if err := os.Remove(filepath.Join(home, path)); err != nil {
			t.Fatal(err)
		}
		next := digest()
		if next == previous {
			t.Fatalf("missed removal of %s", path)
		}
		previous = next
	}
	// The external reference remains observed while absent, so recreation also
	// triggers detection even though its parent is not one of the walked roots.
	write(filepath.Join(home, "external", "body.md"), "recreated instructions")
	if digest() == previous {
		t.Fatal("missed external reference recreation")
	}
}

func TestConfigPollSignalPreservesRejectedGeneration(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.ConfigDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.ConfigDir(), "invalid.yaml"), []byte("kind: Invalid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rejected, err := configDirectoryDigest(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &configReloader{layout: layout, watching: true, appliedDigest: "applied", observedDigest: rejected, rejectedDigest: rejected, rejectionReason: "invalid definition"}
	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := r.poll(now.Add(time.Duration(i) * time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate metadata hiding the rejected edit behind a cached applied
	// generation. An explicit apply must bypass that cache and read disk.
	r.pollDigest.Invalidate()
	if _, err := r.pollDigest.Digest(now, []string{layout.ConfigDir()}, func(func(string)) (string, error) {
		return "applied", nil
	}); err != nil {
		t.Fatal(err)
	}
	if applied, _, _, _, err := r.pollOnce(now); err != nil || applied {
		t.Fatalf("explicit apply: applied=%v err=%v", applied, err)
	}
	status := r.reloadStatus(now)
	if status.State != "rejected" || status.ObservedDigest != rejected || status.AppliedDigest != "applied" || status.RejectionReason != "invalid definition" {
		t.Fatalf("lost rejection fence: %+v", status)
	}
}
