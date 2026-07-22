package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Toolchain de-bashing guard (#630). CI orchestration lives in this Go program
// and the coverage gate in test/coveragegate — not in shell scripts. These
// tests give that property teeth: they fail if a shell (bash/sh/*.sh) creeps
// back onto the build/CI/test path, so the toolchain cannot silently re-bash and
// reintroduce a Unix-shell dependency the Windows port would trip over.

// allowedToolBinaries are the only executables the merge gate may invoke — real
// toolchain binaries resolved on PATH, never a shell interpreter or a project
// script. cmd.exe is permitted solely because Windows npm steps are wrapped
// through the stock command processor (commandInvocation), which is the OS
// launching a real npm — not a project-authored shell script.
var allowedToolBinaries = map[string]bool{
	"go":            true,
	"gofmt":         true,
	"git":           true,
	"npm":           true,
	"golangci-lint": true,
	"cmd.exe":       true,
}

// TestChecksInvokeOnlyAllowlistedToolBinaries proves the merge gate never shells
// out: across every supported GOOS, each check resolves to an allowlisted
// toolchain binary and never to bash/sh or a *.sh script.
func TestChecksInvokeOnlyAllowlistedToolBinaries(t *testing.T) {
	t.Parallel()
	tools := toolchain{
		goCommand:       "go",
		gofmtCommand:    "gofmt",
		gitCommand:      "git",
		npmCommand:      "npm",
		golangciCommand: "golangci-lint",
	}
	metadata := buildMetadata{version: "v0", commit: "c0ffee0", date: "2026-07-21T00:00:00Z"}
	commands := []string{"config-sync", "goobers", "operator", "scheduler"}

	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, current := range checks(commands, tools, metadata, goos, "test-timings/unit.json") {
			binary, _ := commandInvocation(current, goos, func(string) string { return "" })
			base := strings.ToLower(filepath.Base(binary))
			if isShellInterpreter(base) {
				t.Errorf("goos=%s check %q invokes a shell (%q); the merge gate must stay in Go, not shell out", goos, current.label, binary)
			}
			if !allowedToolBinaries[base] {
				t.Errorf("goos=%s check %q invokes unexpected binary %q; add it to allowedToolBinaries only if it is a real toolchain tool, never a shell script", goos, current.label, binary)
			}
		}
	}
}

// TestNoShellScriptsOnToolchainPath fails if any *.sh appears on the build,
// CI, or test path. User-facing sample config (which ships example scripts for
// end users) is exempt — it is not part of the toolchain.
func TestNoShellScriptsOnToolchainPath(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	exemptDirs := []string{
		"config-examples", // end-user sample scripts, not the toolchain
		"portal/node_modules",
		".git",
	}

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			for _, ex := range exemptDirs {
				if rel == ex || strings.HasPrefix(rel, ex+"/") {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".sh") {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module root: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("shell scripts found on the toolchain path (#630 de-bash guard): %v\nMove build/CI/test logic into Go (test/ci, test/coveragegate) instead of shell.", offenders)
	}
}

// TestMakefileGatesDelegateToGo locks the quality gates to their Go
// implementations and prevents recipes from invoking project shell scripts.
func TestMakefileGatesDelegateToGo(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(data)

	for _, want := range []string{
		"run ./test/ci",           // ci: -> the Go merge-gate orchestrator
		"run ./test/ci fast",      // verify-fast: -> the same orchestrator's subset
		"run ./test/ci full",      // verify-full: -> its serialized Make-target mode
		"run ./test/coveragegate", // cover-check: -> the Go coverage gate
		"run ./test/configvalidate",
		"run ./test/integration",
		"run ./test/hermetic", // test: -> the hermetic Go unit-test wrapper
		"run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)",
	} {
		if !strings.Contains(makefile, want) {
			t.Errorf("Makefile no longer delegates to `%s`; the gate must stay in Go, not move into a shell script", want)
		}
	}

	if strings.Contains(makefile, ".sh") {
		t.Error("Makefile references a .sh script; build/CI logic belongs in Go (test/ci, test/coveragegate)")
	}
}

func TestMakefileValidationTiersAreStrictlyNested(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(data)

	tests := []struct {
		target string
		want   makeTarget
	}{
		{
			target: "verify-fast",
			want: makeTarget{
				recipes: []string{"$(GO) run ./test/ci fast"},
			},
		},
		{
			target: "ci",
			want: makeTarget{
				recipes: []string{"$(GO) run ./test/ci"},
			},
		},
		{
			target: "test-integration",
			want: makeTarget{
				recipes: []string{"$(GO) run ./test/integration -go $(GO)"},
			},
		},
		{
			target: "test-integration-strict",
			want: makeTarget{
				recipes: []string{"TESTDEP_STRICT=1 $(GO) run ./test/integration -go $(GO)"},
			},
		},
		{
			target: "vulncheck",
			want: makeTarget{
				recipes: []string{"$(GOVULNCHECK) ./..."},
			},
		},
		{
			target: "verify-full",
			want: makeTarget{
				recipes: []string{`$(GO) run ./test/ci full "$(MAKE)"`},
			},
		},
	}
	for _, test := range tests {
		definitions := makeTargetDefinitions(makefile, test.target)
		if len(definitions) != 1 {
			t.Errorf("%s has %d definitions, want exactly one", test.target, len(definitions))
			continue
		}
		if got := definitions[0]; !slices.Equal(got.prerequisites, test.want.prerequisites) ||
			!slices.Equal(got.recipes, test.want.recipes) {
			t.Errorf("%s = prerequisites %q, recipes %q; want prerequisites %q, recipes %q",
				test.target, got.prerequisites, got.recipes, test.want.prerequisites, test.want.recipes)
		}
	}
}

func TestCIWorkflowUsesValidationMakeTargets(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	workflow := string(data)

	for _, target := range []string{"vulncheck", "test-integration-strict", "test-conformance", "sandbox-check", "linux-node-validation"} {
		if !strings.Contains(workflow, "run: make "+target) {
			t.Errorf("CI workflow must invoke make %s so the job is locally reproducible", target)
		}
	}
	if !strings.Contains(workflow, "needs: [ci, windows-smoke, shipped-workflows, vulnerability-scan, conformance]") {
		t.Error("required CI aggregate must fail when the vulnerability scan or journal conformance gate fails")
	}
}

func TestScheduledVulnerabilityWorkflowUsesMakeTarget(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "vulnerability-scan.yml"))
	if err != nil {
		t.Fatalf("read vulnerability workflow: %v", err)
	}
	workflow := string(data)

	for _, want := range []string{"schedule:", "workflow_dispatch:", "run: make vulncheck"} {
		if !strings.Contains(workflow, want) {
			t.Errorf("scheduled vulnerability workflow must contain %q", want)
		}
	}
}

type makeTarget struct {
	prerequisites []string
	recipes       []string
}

func makeTargetDefinitions(makefile, target string) []makeTarget {
	prefix := target + ":"
	lines := strings.Split(makefile, "\n")
	var definitions []makeTarget
	for i, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		declaration := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		parts := strings.SplitN(declaration, ";", 2)
		definition := makeTarget{
			prerequisites: strings.Fields(parts[0]),
		}
		if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
			definition.recipes = append(definition.recipes, strings.TrimSpace(parts[1]))
		}
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if strings.HasPrefix(next, "\t") {
				definition.recipes = append(definition.recipes, strings.TrimSpace(next))
				continue
			}
			if strings.TrimSpace(next) == "" || strings.HasPrefix(strings.TrimSpace(next), "#") {
				continue
			}
			break
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

// isShellInterpreter reports whether base names a shell interpreter or a shell
// script, the things the toolchain must never invoke.
func isShellInterpreter(base string) bool {
	switch base {
	case "sh", "bash", "zsh", "dash", "ash", "ksh", "csh", "tcsh", "fish", "powershell", "pwsh":
		return true
	}
	return strings.HasSuffix(base, ".sh")
}

// moduleRoot resolves the repository root (the directory holding go.mod) from
// this package directory (test/ci -> ../..).
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	return root
}
