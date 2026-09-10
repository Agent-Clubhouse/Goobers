package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type signingOrderCase struct {
	name    string
	job     string
	markers []string
}

// Publication must depend on execution of the final signed archives. A signing
// success alone cannot detect an executable that never starts on its target OS.
func TestReleasePublicationRequiresNativeArtifactSmoke(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Needs    yaml.Node `yaml:"needs"`
			Strategy struct {
				Matrix struct {
					Include []struct {
						Target string `yaml:"target"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	publication, ok := workflow.Jobs["verify-and-publish"]
	if !ok {
		t.Fatal("missing publication job")
	}
	var dependencies []string
	if err := publication.Needs.Decode(&dependencies); err != nil || !slices.Contains(dependencies, "native-smoke") {
		t.Fatalf("publication must wait for native-smoke: needs=%v error=%v", dependencies, err)
	}
	smoke, ok := workflow.Jobs["native-smoke"]
	if !ok || smoke.Needs.Value != "sign-windows" {
		t.Fatal("native-smoke must consume the final signing job's artifacts")
	}
	var targets []string
	for _, row := range smoke.Strategy.Matrix.Include {
		targets = append(targets, row.Target)
	}
	for _, target := range []string{"darwin_arm64", "darwin_amd64", "windows_amd64", "linux_arm64"} {
		if !slices.Contains(targets, target) {
			t.Errorf("published platform %s has no native smoke leg", target)
		}
	}
	job := workflowJob(string(data), "validate-release")
	markers := []string{
		"- name: Verify release artifacts", "set -euo pipefail",
		"goobers init --allow-ephemeral --demo", "goobers run demo",
		`grep --fixed-strings "phase=completed"`,
		"- name: Exercise installer against staged release assets",
		"- name: Upload validated release notes",
	}
	if marker, ok := firstUnorderedMarker(job, markers); !ok {
		t.Fatalf("publication bypasses executable smoke guard %q", marker)
	}
}

// signingOrderCases pins the order of the release workflow's signing and
// verification steps. Markers name steps and commands only: action versions are
// deliberately left out because test/actionpins already requires an immutable
// commit SHA for every `uses:` in release.yml, and embedding that SHA here
// turned every legitimate pin bump into an unrelated failure of this test.
var signingOrderCases = []signingOrderCase{
	{
		name: "macOS",
		job:  "sign-macos",
		markers: []string{
			"- name: Sign and notarize darwin binaries",
			"codesign --force --options runtime --timestamp",
			`codesign --verify --deep --strict "$WORKDIR/goobers"`,
			`xcrun notarytool submit "$NOTARIZE_ZIP"`,
			// #4269: signing/notarization alone never proves the
			// binary actually runs on its target OS.
			`"$WORKDIR/goobers" --version | grep --fixed-strings "$TAG"`,
			"- name: Recompute SHA256SUMS",
		},
	},
	{
		name: "Windows",
		job:  "sign-windows",
		markers: []string{
			"- name: Sign goobers.exe",
			"uses: azure/trusted-signing-action@",
			"- name: Verify Authenticode signature",
			"if ($certificateOffset -eq 0 -or $certificateSize -eq 0)",
			"$signature = Get-AuthenticodeSignature -FilePath $path",
			"if ($signature.Status -ne 'Valid')",
			// #4269: a valid Authenticode signature alone never proves
			// the exe actually runs.
			"- name: Execute signed goobers.exe",
			"$version = & winsign\\goobers.exe --version",
			"- name: Repackage signed archive",
			"- name: Recompute SHA256SUMS",
		},
	},
}

// firstUnorderedMarker reports the first marker that does not appear after all
// preceding markers in section.
func firstUnorderedMarker(section string, markers []string) (string, bool) {
	remaining := section
	for _, marker := range markers {
		index := strings.Index(remaining, marker)
		if index < 0 {
			return marker, false
		}
		remaining = remaining[index+len(marker):]
	}
	return "", true
}

func TestReleaseWorkflowPreservesSigningVerificationOrder(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(data)

	for _, test := range signingOrderCases {
		t.Run(test.name, func(t *testing.T) {
			job := workflowJob(workflow, test.job)
			if job == "" {
				t.Fatalf("release workflow is missing job %q", test.job)
			}

			if marker, ok := firstUnorderedMarker(job, test.markers); !ok {
				t.Fatalf("job %q must contain %q after the preceding signing markers", test.job, marker)
			}
		})
	}
}

// TestReleaseWorkflowSigningMarkersToleratePinBumps keeps the signing-order
// assertions independent of which action reference release.yml pins, so a
// routine pin bump cannot masquerade as a signing-order regression.
func TestReleaseWorkflowSigningMarkersToleratePinBumps(t *testing.T) {
	t.Parallel()
	commitSHA := regexp.MustCompile(`\b[0-9a-fA-F]{40}\b`)

	for _, test := range signingOrderCases {
		for _, marker := range test.markers {
			if commitSHA.MatchString(marker) {
				t.Errorf("job %q marker %q embeds an action pin; test/actionpins owns pin enforcement", test.job, marker)
			}
		}
	}

	const signWindows = `    steps:
      - name: Sign goobers.exe
        uses: azure/trusted-signing-action@0000000000000000000000000000000000000000
      - name: Verify Authenticode signature
        run: |
          if ($certificateOffset -eq 0 -or $certificateSize -eq 0) { throw 'unsigned' }
          $signature = Get-AuthenticodeSignature -FilePath $path
          if ($signature.Status -ne 'Valid') { throw 'invalid' }
      - name: Execute signed goobers.exe
        run: |
          $version = & winsign\goobers.exe --version
      - name: Repackage signed archive
      - name: Recompute SHA256SUMS
`
	windows, ok := signingOrderCase{}, false
	for _, test := range signingOrderCases {
		if test.job == "sign-windows" {
			windows, ok = test, true
		}
	}
	if !ok {
		t.Fatal("signing order cases must cover job \"sign-windows\"")
	}
	if marker, matched := firstUnorderedMarker(signWindows, windows.markers); !matched {
		t.Errorf("re-pinned sign-windows job must still satisfy marker %q", marker)
	}
}
