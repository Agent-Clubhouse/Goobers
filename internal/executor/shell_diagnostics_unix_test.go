//go:build !windows

package executor

import (
	"strings"
	"testing"
)

// TestFilterProcessTreeLines_NonGoStageCommandsSurvive is the #2172 regression
// test: a hung non-Go process (npm/dotnet/pytest/mvn or java) must survive the
// process-tree keyword filter when its command is threaded in as stageCmd, the
// same way a hung `go test`/`make` process already does.
func TestFilterProcessTreeLines_NonGoStageCommandsSurvive(t *testing.T) {
	tests := []struct {
		name     string
		stageCmd string
		psLine   string
	}{
		{
			name:     "npm",
			stageCmd: "npm",
			psLine:   "  4242  4200  4242 00:05:12 S    npm run ci",
		},
		{
			name:     "dotnet",
			stageCmd: "dotnet",
			psLine:   "  4243  4200  4243 00:05:12 S    dotnet test --logger trx",
		},
		{
			name:     "pytest",
			stageCmd: "pytest",
			psLine:   "  4244  4200  4244 00:05:12 S    pytest -q",
		},
		{
			name:     "mvn",
			stageCmd: "mvn",
			psLine:   "  4245  4200  4245 00:05:12 S    mvn verify",
		},
		{
			name:     "java",
			stageCmd: "java",
			psLine:   "  4246  4200  4246 00:05:12 S    java -jar app-test-runner.jar",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			psOutput := "  PID  PPID   PGID ELAPSED STAT COMMAND\n" + test.psLine + "\n"
			keywords := diagnosticsKeywords(test.stageCmd)
			got := string(filterProcessTreeLines([]byte(psOutput), keywords))
			if !strings.Contains(got, test.psLine) {
				t.Fatalf("filterProcessTreeLines(%q) = %q, want it to contain the hung %s line",
					test.stageCmd, got, test.name)
			}
		})
	}
}

// TestFilterProcessTreeLines_NoRegressionForGoKeywords confirms the
// pre-existing Go-specific keywords (make/go test/.test) still match after
// #2172 folded them into diagnosticsKeywords alongside the stage command,
// even when stageCmd is a completely different (or empty) command.
func TestFilterProcessTreeLines_NoRegressionForGoKeywords(t *testing.T) {
	tests := []struct {
		name   string
		psLine string
	}{
		{name: "make", psLine: "  100  1  100 00:01:00 S    make ci"},
		{name: "go test", psLine: "  101  1  101 00:01:00 S    go test ./..."},
		{name: "compiled .test binary", psLine: "  102  1  102 00:01:00 S    /tmp/goobers.test -test.run TestFoo"},
		{name: "git", psLine: "  103  1  103 00:01:00 S    git fetch origin"},
		{name: "sandbox", psLine: "  104  1  104 00:01:00 S    goobers-sandbox run"},
		{name: "goobers", psLine: "  105  1  105 00:01:00 S    goobers ci-poll"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			psOutput := "  PID  PPID   PGID ELAPSED STAT COMMAND\n" + test.psLine + "\n"
			for _, stageCmd := range []string{"", "npm"} {
				keywords := diagnosticsKeywords(stageCmd)
				got := string(filterProcessTreeLines([]byte(psOutput), keywords))
				if !strings.Contains(got, test.psLine) {
					t.Fatalf("filterProcessTreeLines(stageCmd=%q) = %q, want it to still contain %q",
						stageCmd, got, test.psLine)
				}
			}
		})
	}
}

// TestFilterProcessTreeLines_UnrelatedProcessesDropped confirms the filter
// still drops lines matching none of the keywords — the watchdog's payload
// stays small even after #2172 widened the keyword set.
func TestFilterProcessTreeLines_UnrelatedProcessesDropped(t *testing.T) {
	psOutput := "  PID  PPID   PGID ELAPSED STAT COMMAND\n" +
		"  200  1  200 00:10:00 S    /usr/sbin/some-unrelated-daemon\n" +
		"  201  4200  4200 00:05:00 S    npm run ci\n"
	got := string(filterProcessTreeLines([]byte(psOutput), diagnosticsKeywords("npm")))
	if strings.Contains(got, "some-unrelated-daemon") {
		t.Fatalf("filterProcessTreeLines = %q, want the unrelated daemon line dropped", got)
	}
	if !strings.Contains(got, "npm run ci") {
		t.Fatalf("filterProcessTreeLines = %q, want the stage's npm line kept", got)
	}
}
