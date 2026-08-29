package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseWorkflowPreservesSigningVerificationOrder(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(data)

	tests := []struct {
		name    string
		job     string
		markers []string
	}{
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
				"uses: azure/trusted-signing-action@c7ab2a863ab5f9a846ddb8265964877ef296ee82",
				"- name: Verify Authenticode signature",
				"if ($certificateOffset -eq 0 -or $certificateSize -eq 0)",
				"$signature = Get-AuthenticodeSignature -FilePath $path",
				"if ($signature.Status -ne 'Valid')",
				"- name: Repackage signed archive",
				"- name: Recompute SHA256SUMS",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := workflowJob(workflow, test.job)
			if job == "" {
				t.Fatalf("release workflow is missing job %q", test.job)
			}

			remaining := job
			for _, marker := range test.markers {
				index := strings.Index(remaining, marker)
				if index < 0 {
					t.Fatalf("job %q must contain %q after the preceding signing markers", test.job, marker)
				}
				remaining = remaining[index+len(marker):]
			}
		})
	}
}
