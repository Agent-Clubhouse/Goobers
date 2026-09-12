package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackagedOnboardingUsesInstallRouteCommand(t *testing.T) {
	repoRoot := gitOutput("rev-parse", "--show-toplevel")
	if repoRoot == "" {
		t.Fatal("resolve repository root")
	}
	tests := []struct {
		name          string
		version       string
		command       string
		confirmation  string
		installRoute  string
		forbidCommand string
	}{
		{
			name:          "stable installer",
			version:       "v1.2.3",
			command:       "goobers-v1.2.3",
			confirmation:  "## Confirm the installed binary",
			installRoute:  "The release installer installs the binary and documentation only",
			forbidCommand: "goobers-v1.2.3-rc.1",
		},
		{
			name:          "prerelease manual extraction",
			version:       "v1.2.3-rc.1",
			command:       "./goobers",
			confirmation:  "## Confirm the extracted binary",
			installRoute:  "distributed for manual archive extraction",
			forbidCommand: "goobers-v1.2.3-rc.1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payloadDir := t.TempDir()
			for _, rel := range []string{
				"README.md",
				"docs/guides/quickstart.md",
				"docs/guides/quickstart-linux.md",
			} {
				destination := filepath.Join(payloadDir, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := copyReleaseFile(filepath.Join(repoRoot, filepath.FromSlash(rel)), destination); err != nil {
					t.Fatal(err)
				}
			}

			if err := adaptInstalledOnboarding(payloadDir, tc.version); err != nil {
				t.Fatalf("adaptInstalledOnboarding: %v", err)
			}
			for _, rel := range []string{"README.md", "docs/guides/quickstart.md"} {
				data, err := os.ReadFile(filepath.Join(payloadDir, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				content := string(data)
				for _, suffix := range []string{
					" --version",
					" init --demo ./demo-instance",
					" run demo ./demo-instance",
					" init --guided",
				} {
					if !strings.Contains(content, tc.command+suffix) {
						t.Errorf("%s missing install-route command %q", rel, tc.command+suffix)
					}
				}
				if strings.Contains(content, tc.forbidCommand) {
					t.Errorf("%s documents unavailable command %q", rel, tc.forbidCommand)
				}
			}
			quickstart, err := os.ReadFile(filepath.Join(payloadDir, "docs/guides/quickstart.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(quickstart), tc.confirmation) {
				t.Errorf("quickstart missing confirmation %q", tc.confirmation)
			}

			readme, err := os.ReadFile(filepath.Join(payloadDir, "README.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(readme), tc.installRoute) {
				t.Errorf("README.md missing install route %q", tc.installRoute)
			}
		})
	}
}
