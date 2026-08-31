package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type signingOrderCase struct {
	name    string
	job     string
	markers []string
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
