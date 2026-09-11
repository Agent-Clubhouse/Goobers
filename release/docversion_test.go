package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGuide(t *testing.T, guidesDir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(guidesDir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckShippedGuideVersionPinsRejectsStaleLiteral(t *testing.T) {
	repoRoot := t.TempDir()
	guidesDir := filepath.Join(repoRoot, "docs", "guides")
	if err := os.MkdirAll(guidesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The exact #4831 repro: a stable, checksummable release four minors
	// behind supportmatrix.NextPlannedRelease (v0.4.0), hardcoded rather than
	// resolved dynamically.
	writeGuide(t, guidesDir, "quickstart-macos.md", "```sh\nVERSION=v0.1.0\n...\n```\n")

	err := checkShippedGuideVersionPins(repoRoot)
	if err == nil || !strings.Contains(err.Error(), "quickstart-macos.md pins release version v0.1.0") {
		t.Fatalf("checkShippedGuideVersionPins() = %v, want a refusal naming quickstart-macos.md and v0.1.0", err)
	}
}

func TestCheckShippedGuideVersionPinsAllowsDynamicResolution(t *testing.T) {
	repoRoot := t.TempDir()
	guidesDir := filepath.Join(repoRoot, "docs", "guides")
	if err := os.MkdirAll(guidesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGuide(t, guidesDir, "quickstart-macos.md", "```sh\n"+
		"VERSION=\"$(curl -fsSL https://api.github.com/repos/Agent-Clubhouse/Goobers/releases/latest |\n"+
		"  awk -F '\"' '/tag_name/ { print $4; exit }')\"\n"+
		"...\n```\n")

	if err := checkShippedGuideVersionPins(repoRoot); err != nil {
		t.Fatalf("checkShippedGuideVersionPins() = %v, want nil for dynamic resolution", err)
	}
}

func TestCheckShippedGuideVersionPinsAllowsCurrentOrNewerLiteral(t *testing.T) {
	repoRoot := t.TempDir()
	guidesDir := filepath.Join(repoRoot, "docs", "guides")
	if err := os.MkdirAll(guidesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A literal naming the current planned release line (or newer) is not a
	// staleness bug — e.g. a guide intentionally pinned to a specific tag for
	// reproducibility.
	writeGuide(t, guidesDir, "pinned-example.md", "```sh\nVERSION=v0.4.0\n```\n")
	writeGuide(t, guidesDir, "no-version.md", "Nothing to see here.\n")

	if err := checkShippedGuideVersionPins(repoRoot); err != nil {
		t.Fatalf("checkShippedGuideVersionPins() = %v, want nil for a current-or-newer literal", err)
	}
}
