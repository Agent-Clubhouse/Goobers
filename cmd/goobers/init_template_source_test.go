package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"

	"github.com/goobers/goobers/api/schemas"
)

func TestInitQuickstartConfigSourceJSONGoldens(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
		check func(*testing.T, string)
	}{
		{name: "empty"},
		{
			name: "partial",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
					t.Fatalf("seed partial fixture: %v", err)
				}
				if err := os.Remove(filepath.Join(root, instance.GuidedSourceInstanceFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(filepath.Join(root, "gaggles")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "populated",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
					t.Fatalf("seed populated fixture: %v", err)
				}
				if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("keep me\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, root string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(root, "README.md"))
				if err != nil || string(data) != "keep me\n" {
					t.Fatalf("unmanaged file changed: data=%q err=%v", data, err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "config-source")
			if test.setup != nil {
				test.setup(t, root)
			}

			code, stdout, stderr := runArgs(
				t,
				"init",
				"--template=quickstart",
				"--source-tree",
				root,
				"--json",
			)
			if code != 0 || stderr != "" {
				t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if test.check != nil {
				test.check(t, root)
			}
			// The quickstart-v1 template ships its own gaggle-scoped
			// implement/run-tests/review skill packages (SKILL002 fix); a
			// shared-level stand-in here would collide with them (SKILL001).

			var envelope onboardingActionResult
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("decode result: %v\n%s", err, stdout)
			}
			envelope.Path = "$SOURCE"
			envelope.NextCommand = strings.ReplaceAll(envelope.NextCommand, absolutePath(root), "$SOURCE")
			var normalized bytes.Buffer
			if err := encodeSchemaJSON(&normalized, schemas.OnboardingAction, envelope); err != nil {
				t.Fatal(err)
			}
			assertGoldenFile(
				t,
				filepath.Join("testdata", "init-template-source", test.name+".golden.json"),
				normalized.String(),
			)

			assertQuickstartSourceValid(t, root)
		})
	}
}

func TestInitQuickstartConfigSourceRejectsConflictingManagedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config-source")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "manifest.yaml")
	if err := os.WriteFile(manifest, []byte("user-owned\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(
		t,
		"init",
		"--template=quickstart",
		"--source-tree",
		root,
		"--json",
	)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "manifest.yaml differs from the quickstart template") {
		t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	data, err := os.ReadFile(manifest)
	if err != nil || string(data) != "user-owned\n" {
		t.Fatalf("conflicting manifest changed: data=%q err=%v", data, err)
	}
}

func TestInitQuickstartConfigSourceRejectsSemanticallyInvalidPopulatedDestination(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config-source")
	if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
		t.Fatalf("seed populated fixture: %v", err)
	}
	workflowPath := filepath.Join(root, "gaggles", "example", "workflows", "quickstart.yaml")
	workflow, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	invalidWorkflow := strings.Replace(string(workflow), "name: quickstart", "name: invalid-command", 1)
	invalidWorkflow = strings.Replace(invalidWorkflow, `"backlog-query"`, `"missing-command"`, 1)
	if err := os.WriteFile(
		filepath.Join(root, "gaggles", "example", "workflows", "invalid-command.yaml"),
		[]byte(invalidWorkflow),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(
		t,
		"init",
		"--template=quickstart",
		"--source-tree",
		root,
		"--json",
	)
	// Since the C+D2/#2861 wave the unknown verb is rejected during the
	// api/validate pass (WF010, the DSL compilers' admission check against
	// internal/builtincmd) — earlier than the late #650 COMMAND001 pass this
	// case used to reach, so it surfaces as a seeding-validation error (exit
	// 2) rather than the late pass's JSON diagnostics (exit 1).
	if code != 2 || stdout != "" ||
		!strings.Contains(stderr, "WF010") ||
		!strings.Contains(stderr, "unknown built-in subcommand") ||
		!strings.Contains(stderr, "missing-command") {
		t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestInitQuickstartConfigSourceQuotesNextCommandPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config $HOME ' $(touch-pwned) `touch-pwned`")
	code, stdout, stderr := runArgs(
		t,
		"init",
		"--template=quickstart",
		"--source-tree",
		root,
		"--json",
	)
	if code != 0 || stderr != "" {
		t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var envelope onboardingActionResult
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode result: %v\n%s", err, stdout)
	}
	abs := absolutePath(root)
	// nextCommand's quoting matches the host shell: quoteShellArg branches on
	// runtime.GOOS, using PowerShell ''-doubling on Windows (that command is
	// meant to be pasted into the user's actual shell — PowerShell on
	// Windows, POSIX sh/bash elsewhere) and POSIX '"'"'-escaping everywhere
	// else. This test runs with the real host GOOS via runArgs/runInit (no
	// forced OS), so its expectation must follow the same branch;
	// TestInitQuickstartConfigSourceQuotesWindowsNextCommandPath covers the
	// Windows-quoting behavior explicitly (forced goos="windows") regardless
	// of host.
	quotedAbs := "'" + strings.ReplaceAll(abs, "'", `'"'"'`) + "'"
	if runtime.GOOS == "windows" {
		quotedAbs = "'" + strings.ReplaceAll(abs, "'", "''") + "'"
	}
	want := "goobers validate --source-tree --json " + quotedAbs
	if envelope.NextCommand != want {
		t.Fatalf("nextCommand = %q, want %q", envelope.NextCommand, want)
	}
}

func TestInitQuickstartConfigSourceQuotesWindowsNextCommandPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config $HOME $([char]0x58) `n O'Brien")
	var stdout, stderr bytes.Buffer
	code := runInitWithInputForOS(
		[]string{
			"--template=quickstart",
			"--source-tree",
			root,
			"--json",
		},
		strings.NewReader(""),
		&stdout,
		&stderr,
		"windows",
	)
	if code != 0 || stderr.String() != "" {
		t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var envelope onboardingActionResult
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode result: %v\n%s", err, stdout.String())
	}
	want := "goobers validate --source-tree --json '" +
		strings.ReplaceAll(absolutePath(root), "'", "''") + "'"
	if envelope.NextCommand != want {
		t.Fatalf("nextCommand = %q, want %q", envelope.NextCommand, want)
	}
}

func TestQuoteShellArgPowerShellLiteralSemantics(t *testing.T) {
	var shell string
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			shell = path
			break
		}
	}
	if shell == "" {
		t.Skip("PowerShell is not installed")
	}

	arg := `C:\config\$HOME $([char]0x58) ` + "`n O'Brien"
	script := "[Console]::Out.Write(" + quoteShellArg(arg, "windows") + ")"
	output, err := exec.Command(
		shell,
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		script,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("evaluate quoted argument: %v\n%s", err, output)
	}
	if string(output) != arg {
		t.Fatalf("PowerShell argument = %q, want %q", output, arg)
	}
}

func TestInitQuickstartConfigSourceFlagValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	tests := []struct {
		args []string
		want string
	}{
		{
			args: []string{"init", "--template=quickstart", "--source-tree="},
			want: "--source-tree destination must not be empty",
		},
		{
			args: []string{"init", "--template=quickstart", "--source-tree", ""},
			want: "--source-tree destination must not be empty",
		},
		{
			args: []string{"init", "--source-tree", "source"},
			want: "--source-tree requires --template=quickstart",
		},
		{
			args: []string{"init", "--json"},
			want: "--json is supported by init only with --source-tree",
		},
		{
			args: []string{"init", "--template=quickstart", "--source-tree", "source", "instance"},
			want: "--source-tree supplies the destination",
		},
	}
	for _, test := range tests {
		code, stdout, stderr := runArgs(t, test.args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, test.want) {
			t.Errorf("args=%v code=%d stdout=%q stderr=%q, want %q", test.args, code, stdout, stderr, test.want)
		}
	}
}

// TestInitQuickstartHarnessSelection covers #3071: --harness makes the seeded
// quickstart instance and config source use the chosen harness with no manual
// goober.yaml edit, and the option is refused outside the quickstart template
// or with an unknown harness name.
func TestInitQuickstartHarnessSelection(t *testing.T) {
	t.Run("instance", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "instance")
		code, stdout, stderr := runArgs(t, "init", "--template=quickstart", "--harness", "claude-code", root)
		if code != 0 {
			t.Fatalf("init: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertSeededHarness(t, instance.NewLayout(root).ConfigDir(), "claude-code")
	})

	t.Run("source tree", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "config-source")
		code, stdout, stderr := runArgs(
			t,
			"init",
			"--template=quickstart",
			"--harness",
			"claude-code",
			"--source-tree",
			root,
		)
		if code != 0 || stderr != "" {
			t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertSeededHarness(t, root, "claude-code")
		assertQuickstartSourceValid(t, root)
	})

	t.Run("default preserved", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "config-source")
		code, stdout, stderr := runArgs(t, "init", "--template=quickstart", "--source-tree", root)
		if code != 0 || stderr != "" {
			t.Fatalf("init source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertSeededHarness(t, root, "copilot")
	})

	t.Run("flag validation", func(t *testing.T) {
		t.Chdir(t.TempDir())
		tests := []struct {
			args []string
			want string
		}{
			{
				args: []string{"init", "--harness", "claude-code"},
				want: "--harness requires --template=quickstart",
			},
			{
				args: []string{"init", "--demo", "--harness", "claude-code"},
				want: "--harness requires --template=quickstart",
			},
			{
				args: []string{"init", "--template=quickstart", "--harness", "nope", "instance"},
				want: `harness must be "copilot" or "claude-code"`,
			},
		}
		for _, test := range tests {
			code, stdout, stderr := runArgs(t, test.args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Errorf("args=%v code=%d stdout=%q stderr=%q, want %q", test.args, code, stdout, stderr, test.want)
			}
		}
	})
}

func assertSeededHarness(t *testing.T, configDir, want string) {
	t.Helper()
	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if len(set.Goobers) == 0 {
		t.Fatalf("no goobers seeded under %s", configDir)
	}
	for _, goober := range set.Goobers {
		if string(goober.Spec.Harness) != want {
			t.Fatalf("goober %q harness = %q, want %q", goober.Name, goober.Spec.Harness, want)
		}
	}
}

func assertQuickstartSourceValid(t *testing.T, root string) {
	t.Helper()
	code, stdout, stderr := runArgs(t, "validate", "--source-tree", "--json", root)
	if code != 0 || stderr != "" {
		t.Fatalf("validate source: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result struct {
		OK       bool                `json:"ok"`
		Findings []diagnosticFinding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode validation result: %v\n%s", err, stdout)
	}
	if !result.OK || len(result.Findings) != 2 {
		t.Fatalf("validation result = %s", stdout)
	}
	for _, finding := range result.Findings {
		if finding.Code != placeholderFindingCode || finding.Severity != "warning" {
			t.Fatalf("quickstart source has non-placeholder finding %+v", finding)
		}
	}
}
