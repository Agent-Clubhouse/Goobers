package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stagePackagedDocs reproduces the payload's onboarding rewrite for version.
func stagePackagedDocs(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	if err := copyReleaseTree("../docs", filepath.Join(dir, "docs")); err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), readme, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := adaptInstalledOnboarding(dir, version); err != nil {
		t.Fatalf("adapt onboarding for %s: %v", version, err)
	}
	return dir
}

// The release workflow greps the packaged quickstart for install wording, and
// #4978 changed that wording for pre-releases without updating the greps —
// which made the release job unsatisfiable for exactly the tags an RC uses and
// blocked v0.4.0-rc.3 rather than catching anything.
//
// These assertions mirror the workflow's, so the generator and the gate that
// reads it can no longer drift apart silently: a change to either fails here,
// in a second, instead of after a full signed release build.
func TestPackagedQuickstartWordingMatchesTheReleaseGate(t *testing.T) {
	tests := []struct {
		name, version   string
		present, absent []string
	}{
		{
			name:    "stable installs from PATH",
			version: "v0.4.0",
			present: []string{
				"bundled with release `v0.4.0`",
				"goobers-v0.4.0 init --guided",
				"goobers-v0.4.0 run implementation ./my-instance",
			},
			absent: []string{"This pre-release `v0.4.0`"},
		},
		{
			// install.sh refuses pre-release tags, so the guide must point at
			// the extracted archive rather than a binary nothing installed.
			name:    "pre-release runs the extracted archive",
			version: "v0.4.0-rc.3",
			present: []string{
				"This pre-release `v0.4.0-rc.3`",
				"./goobers --version",
				"./goobers init --demo ./demo-instance",
			},
			absent: []string{
				"bundled with release `v0.4.0-rc.3`",
				"goobers-v0.4.0-rc.3 init --guided",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := stagePackagedDocs(t, test.version)
			raw, err := os.ReadFile(filepath.Join(dir, "docs", "guides", "quickstart.md"))
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			for _, want := range test.present {
				if !strings.Contains(body, want) {
					t.Errorf("packaged quickstart for %s is missing %q, which the release workflow greps for",
						test.version, want)
				}
			}
			for _, unwanted := range test.absent {
				if strings.Contains(body, unwanted) {
					t.Errorf("packaged quickstart for %s carries %q, which belongs to the other tag shape",
						test.version, unwanted)
				}
			}
		})
	}
}
