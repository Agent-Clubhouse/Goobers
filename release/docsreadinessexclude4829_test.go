package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExcludedReleaseDocMatchesOnlyReadinessRecords is #4829's core
// regression: a per-candidate readiness scratchpad (citing one author's own
// /tmp evidence trail, asserting promotion status that goes stale the moment
// a later candidate ships) must never reach a release archive, while other
// docs/releases content — release notes meant for end users — still does.
func TestExcludedReleaseDocMatchesOnlyReadinessRecords(t *testing.T) {
	tests := []struct {
		rel  string
		want bool
	}{
		{rel: "releases/v0.4.0-rc.1-readiness.md", want: true},
		{rel: "releases/v1.2.3-readiness.md", want: true},
		{rel: "releases/sample-release-notes.md", want: false},
		{rel: "releases/README.md", want: false},
		// Only the releases/ directory is scoped — a same-named file
		// elsewhere in docs/ is not a working scratchpad.
		{rel: "design/v0.4.0-rc.1-readiness.md", want: false},
		{rel: "guides/readiness.md", want: false},
	}
	for _, test := range tests {
		t.Run(test.rel, func(t *testing.T) {
			if got := excludedReleaseDoc(filepath.FromSlash(test.rel)); got != test.want {
				t.Fatalf("excludedReleaseDoc(%q) = %v, want %v", test.rel, got, test.want)
			}
		})
	}
}

// TestCopyReleaseTreeExcludesReadinessRecords is the integration-level
// regression for #4829: v0.4.0-rc.1-readiness.md (and any future
// *-readiness.md) must not appear in a staged release payload, while
// unrelated docs/releases content is staged normally.
func TestCopyReleaseTreeExcludesReadinessRecords(t *testing.T) {
	source := t.TempDir()
	releasesDir := filepath.Join(source, "releases")
	if err := os.MkdirAll(releasesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(releasesDir, "v0.4.0-rc.1-readiness.md"): "Promotion remains blocked. Evidence in /tmp/goobers-rc-x.",
		filepath.Join(releasesDir, "sample-release-notes.md"):  "# Goobers v0.2.0",
		filepath.Join(source, "guides", "quickstart.md"):       "quickstart",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	destination := t.TempDir()
	if err := copyReleaseTree(source, destination); err != nil {
		t.Fatalf("copyReleaseTree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(destination, "releases", "v0.4.0-rc.1-readiness.md")); !os.IsNotExist(err) {
		t.Fatalf("readiness record staged into the release payload (err = %v), want excluded", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "releases", "sample-release-notes.md")); err != nil {
		t.Fatalf("release notes missing from the release payload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "guides", "quickstart.md")); err != nil {
		t.Fatalf("unrelated doc missing from the release payload: %v", err)
	}
}
