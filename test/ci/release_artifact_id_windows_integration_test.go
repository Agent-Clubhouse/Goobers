//go:build integration && windows

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationReleaseWindowsArtifactIDGuardPreventsDownloadAllFallback(t *testing.T) {
	testdep.Require(t, "powershell.exe")
	workflow := loadReleaseAuthorizationWorkflow(t)
	var script string
	for _, step := range workflow.Jobs["native-windows-image"].Steps {
		if step.Name == "Validate final signed artifact ID" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing Windows identity guard")
	}
	for _, tc := range releaseArtifactIDCases {
		t.Run(tc.value, func(t *testing.T) {
			// The guard uses shared PowerShell syntax supported by both the runner's
			// pwsh and stock Windows PowerShell. Test the latter without adding a tool.
			command := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script+"\nWrite-Output 'DOWNLOAD_ACTION_ENTERED'\n")
			command.Env = append(os.Environ(), "ARTIFACT_ID="+tc.value)
			output, err := command.CombinedOutput()
			entered := strings.Contains(string(output), "DOWNLOAD_ACTION_ENTERED")
			if (err == nil) != tc.allowed || entered != tc.allowed {
				t.Fatalf("ID guard allowed=%v entered=%v, want %v: %v\n%s", err == nil, entered, tc.allowed, err, output)
			}
		})
	}
}
