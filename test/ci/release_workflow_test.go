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
	if !ok || smoke.Needs.Value != "assemble" {
		t.Fatal("native-smoke must consume the assembled final signed artifacts")
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
	smokeJob := workflowJob(string(data), "native-smoke")
	for _, evidence := range []string{"$RUNNER_ARCH", "selected-checksum)", "$GITHUB_STEP_SUMMARY"} {
		if !strings.Contains(smokeJob, evidence) {
			t.Errorf("native-smoke must record runner arch and tested digest; missing %q", evidence)
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

func TestReleaseBuildPublishesReusablePortalPackage(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	build := workflowJob(string(data), "build")
	markers := []string{
		"- name: Build Portal artifacts",
		`export GOOBERS_PORTAL_PACKAGE_VERSION="${GITHUB_REF_NAME#v}"`,
		"npm --prefix portal run package:portal",
		"npm --prefix portal run test:package",
		`portal_package="goobers-portal-${portal_version}.tgz"`,
		`cp "portal/.portal-package/$portal_package" dist/`,
		"cp portal/.portal-package/package/portal-artifact.json dist/",
		`sha256sum "$portal_package" "$portal_package.sha256" portal-artifact.json >> SHA256SUMS`,
	}
	if marker, ok := firstUnorderedMarker(build, markers); !ok {
		t.Fatalf("release build does not publish the reusable Portal package: missing or unordered %q", marker)
	}
	validation := workflowJob(string(data), "validate-release")
	for _, required := range []string{
		`(cd dist && sha256sum --check "$portal_package.sha256")`,
		`.packageVersion == $version and .commit == $commit and .dirty == false`,
		`tar -xOf "dist/$portal_package" package/portal-artifact.json`,
	} {
		if !strings.Contains(validation, required) {
			t.Errorf("release validation lacks reusable Portal check %q", required)
		}
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
			`tar -czf "signed/$(basename "$ARCHIVE")"`,
			"- name: Upload signed darwin archives",
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
			"- name: Upload signed Windows archive",
		},
	},
	{
		// #5413: SHA256SUMS is recomputed once, after both signers, and only
		// then uploaded as the final set every signed-byte gate consumes.
		name: "assemble",
		job:  "assemble",
		markers: []string{
			"- name: Download unsigned artifacts",
			"- name: Download signed darwin archives",
			"- name: Download signed Windows archive",
			"- name: " + releaseAssembleStep,
			"sha256sum --check --strict --quiet SHA256SUMS",
			`cp "$signed_file" "dist/$name"`,
			"xargs sha256sum --",
			"its signer did not re-pack it",
			"changed between build and publish but is never signed",
			"- name: Upload final signed artifacts",
		},
	},
}

// releaseAssembleStep names the one step that merges signed archives into the
// build's set and recomputes SHA256SUMS.
const releaseAssembleStep = "Merge signed archives and recompute SHA256SUMS"

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
      - name: Upload signed Windows archive
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

// TestReleaseSignersUploadOnlyTheirArchives pins #5413's parallel-signing
// contract: each signer starts from the build, uploads only the archive(s) it
// re-packed, and leaves SHA256SUMS to assemble, so neither signer can carry
// (or rewrite) the other's output or the shared manifest.
func TestReleaseSignersUploadOnlyTheirArchives(t *testing.T) {
	t.Parallel()
	workflow := loadReleaseAuthorizationWorkflow(t)
	for job, artifact := range map[string]string{"sign-macos": "dist-signed-darwin", "sign-windows": "dist-signed-windows"} {
		uploads := 0
		for _, step := range workflow.Jobs[job].Steps {
			if strings.Contains(step.Run, "SHA256SUMS") {
				t.Errorf("%s step %q must not read or rewrite SHA256SUMS; assemble recomputes it once", job, step.Name)
			}
			if step.With["name"] == "dist-unsigned" && step.With["path"] != "dist" {
				t.Errorf("%s must sign from the build's upload", job)
			}
			if _, ok := step.With["if-no-files-found"]; !ok {
				continue
			}
			uploads++
			if step.With["name"] != artifact || step.With["path"] != "signed/" || step.With["overwrite"] != "" {
				t.Errorf("%s must upload only its signed archives as %s: %v", job, artifact, step.With)
			}
		}
		if uploads != 1 {
			t.Errorf("%s must make exactly one upload, got %d", job, uploads)
		}
	}
}
