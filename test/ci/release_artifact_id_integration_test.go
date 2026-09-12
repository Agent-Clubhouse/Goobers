//go:build integration

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

var releaseArtifactIDCases = []struct {
	value   string
	allowed bool
}{
	{"1", true}, {"9876543210", true},
	{"", false}, {"0", false}, {"01", false}, {"-1", false}, {"+1", false},
	{"1,2", false}, {"1 2", false}, {"1\n", false}, {"1\r\n", false},
	{" 1", false}, {"1 ", false}, {"1.0", false}, {"1e2", false},
	{"abc", false}, {"١", false}, {"$(exit 0)", false},
}

func TestIntegrationReleaseArtifactIDGuardsPreventDownloadAllFallback(t *testing.T) {
	testdep.Require(t, "bash")
	workflow := loadReleaseAuthorizationWorkflow(t)
	checked := 0
	for jobName, job := range workflow.Jobs {
		for index, step := range job.Steps {
			if step.With["artifact-ids"] == "" || index == 0 {
				continue
			}
			guard := job.Steps[index-1]
			if guard.Shell != "bash" {
				continue
			}
			checked++
			for _, tc := range releaseArtifactIDCases {
				t.Run(jobName+"/"+guard.Name+"/"+tc.value, func(t *testing.T) {
					// The marker models entering the immediately following download action.
					// Missing/CSV IDs must fail before its download-all fallback can run.
					script := guard.Run + "\nprintf '%s' 'DOWNLOAD_ACTION_ENTERED'\n"
					command := exec.Command("bash", "--noprofile", "--norc", "-c", script)
					command.Env = append(os.Environ(), "ARTIFACT_ID="+tc.value)
					output, err := command.CombinedOutput()
					entered := strings.Contains(string(output), "DOWNLOAD_ACTION_ENTERED")
					if (err == nil) != tc.allowed || entered != tc.allowed {
						t.Fatalf("ID guard allowed=%v entered=%v, want %v: %v\n%s", err == nil, entered, tc.allowed, err, output)
					}
				})
			}
		}
	}
	if checked != 5 {
		t.Fatalf("expected five Bash identity download guards, got %d", checked)
	}
}
