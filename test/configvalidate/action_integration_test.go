//go:build integration

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Execute the actual composite step, not a second implementation of its shell
// logic. The release installer is deliberately excluded; no network is needed.
func TestIntegrationValidateActionWarningAllowlist(t *testing.T) {
	testdep.Require(t, "bash")
	script := validationActionScript(t)
	for _, tc := range []struct {
		name, expected, stdout, stderr string
		strict, unset, missing         bool
		captureFailure                 bool
		validatorCode, wantCode        int
		want                           string
	}{
		{name: "exact unordered", expected: "WARNING b\nWARNING a\n", stdout: "WARNING a\nINFO ok\nWARNING b\n"},
		{name: "new", expected: "WARNING a\n", stdout: "WARNING a\nWARNING b\n", wantCode: 1, want: "+WARNING b"},
		{name: "stale", expected: "WARNING a\nWARNING b\n", stdout: "WARNING a\n", wantCode: 1, want: "-WARNING b"},
		{name: "hard error matching warnings", expected: "WARNING a\n", stdout: "WARNING a\n", stderr: "ERROR invalid\n", validatorCode: 7, wantCode: 7, want: "ERROR invalid"},
		{name: "empty"},
		{name: "empty rejects warning", stdout: "WARNING a\n", wantCode: 1},
		{name: "stderr warning", expected: "WARNING a\n", stderr: "WARNING a\n"},
		{name: "duplicate matters", expected: "WARNING a\n", stdout: "WARNING a\nWARNING a\n", wantCode: 1},
		{name: "missing file", missing: true, wantCode: 1, want: "readable regular file"},
		{name: "strict conflict", strict: true, wantCode: 1, want: "mutually exclusive"},
		{name: "unset permits warnings", unset: true, stdout: "WARNING a\n"},
		{name: "unset retains error", unset: true, validatorCode: 9, wantCode: 9},
		{name: "unset strict forwards flag", unset: true, strict: true},
		{name: "capture failure fails closed", captureFailure: true, wantCode: 1, want: "could not capture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			allowlist := filepath.Join(root, "expected warnings.txt")
			if !tc.missing {
				if err := os.WriteFile(allowlist, []byte(tc.expected), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.unset {
				allowlist = ""
			}
			// A shell function replaces only the validator. It also verifies that
			// user paths remain one argument, including spaces and shell syntax.
			fake := `goobers() {
  local expected=(validate --source-tree --github-annotations)
  if [ "$GOOBERS_VALIDATE_STRICT" = true ]; then expected+=(--strict); fi
  expected+=("$GOOBERS_VALIDATE_PATH")
  [ "$#" -eq "${#expected[@]}" ] || return 80
  local arg
  for arg in "${expected[@]}"; do [ "$1" = "$arg" ] || return 81; shift; done
  printf '%s' "$FAKE_STDOUT"
  printf '%s' "$FAKE_STDERR" >&2
  return "$FAKE_CODE"
}
tee() {
  if [ "$FAKE_TEE_FAIL" = true ]; then return 23; fi
  command tee "$@"
}
`
			cmd := exec.Command("bash", "-c", fake+script)
			cmd.Env = append(os.Environ(),
				"GOOBERS_VALIDATE_PATH=config with spaces; $(false)",
				fmt.Sprintf("GOOBERS_VALIDATE_STRICT=%t", tc.strict),
				"GOOBERS_ALLOWED_WARNINGS="+allowlist, "RUNNER_TEMP="+root,
				"FAKE_STDOUT="+tc.stdout, "FAKE_STDERR="+tc.stderr,
				fmt.Sprintf("FAKE_TEE_FAIL=%t", tc.captureFailure),
				fmt.Sprintf("FAKE_CODE=%d", tc.validatorCode))
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if cmd.ProcessState == nil {
					t.Fatal(err)
				}
				code = cmd.ProcessState.ExitCode()
			}
			if code != tc.wantCode || !strings.Contains(string(out), tc.want) {
				t.Fatalf("code=%d want=%d, output=%s (want %q)", code, tc.wantCode, out, tc.want)
			}
			left, err := filepath.Glob(filepath.Join(root, "goobers-warnings.*"))
			if err != nil || len(left) != 0 {
				t.Fatalf("scratch files leaked: %v, %v", left, err)
			}
		})
	}
}

func validationActionScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "actions", "validate", "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Inputs map[string]any `yaml:"inputs"`
		Runs   struct {
			Steps []struct {
				Name, Run string
				Env       map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatal(err)
	}
	if _, ok := action.Inputs["allowed-warnings-file"]; !ok {
		t.Fatal("allowlist input missing")
	}
	for _, step := range action.Runs.Steps {
		if step.Name == "goobers validate --source-tree" {
			if step.Env["GOOBERS_ALLOWED_WARNINGS"] != "${{ inputs.allowed-warnings-file }}" {
				t.Fatal("allowlist input is not wired to the validation step")
			}
			return step.Run
		}
	}
	t.Fatal("validation step missing")
	return ""
}
