// Command configvalidate runs the built validator over every checked-in config.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const instanceYAML = `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GOOBERS_GITHUB_TOKEN
  - provider: github
    owner: acme
    name: dotnet-service
    token:
      env: GOOBERS_GITHUB_TOKEN
  - provider: github
    owner: acme
    name: java-service
    token:
      env: GOOBERS_GITHUB_TOKEN
  - provider: github
    owner: acme
    name: python-service
    token:
      env: GOOBERS_GITHUB_TOKEN
  - provider: github
    owner: acme
    name: ios-app
    token:
      env: GOOBERS_GITHUB_TOKEN
  - provider: github
    owner: your-org
    name: your-repo
    token:
      env: GOOBERS_GITHUB_TOKEN
  - provider: github
    owner: demo
    name: offline
    token:
      env: GOOBERS_GITHUB_TOKEN
  - provider: github
    owner: example
    name: example
    token:
      env: GOOBERS_GITHUB_TOKEN
`

const docsUpdaterInertWarning = "WARNING Workflow/docs-updater: workflow \"docs-updater\" has no schedule trigger; it will not fire autonomously \u2014 run it with `goobers run docs-updater`"

const referenceWorkflowDocsRootWarning = "WARNING DOCS003 Workflow/docs-updater: declared docs root %q not verified: config tree is not the target repository Agent-Clubhouse/Goobers"

type checkedInTree struct {
	path            string
	sourceTree      bool
	strict          bool
	allowedWarnings []string
}

var checkedInTrees = []checkedInTree{
	{
		path:       "reference-workflows",
		sourceTree: true,
		strict:     true,
		allowedWarnings: []string{
			docsUpdaterInertWarning,
			fmt.Sprintf(referenceWorkflowDocsRootWarning, "docs"),
			fmt.Sprintf(referenceWorkflowDocsRootWarning, "README.md"),
		},
	},
	{path: "config-examples"},
	{path: "examples/ios-simulator"},
	{path: "internal/instance/starter"},
	{path: "internal/instance/demo"},
	{path: "test/fixtures/e2e/walking-skeleton"},
	// The #687 config-repo PR validation gate's passing self-test fixture
	// (.github/actions/validate, docs/guides/config-pr-validation-gate.md):
	// keeps it from silently rotting out of sync with the validator it
	// exists to demonstrate. Its sibling "invalid" fixture is deliberately
	// broken and is exercised by TestValidateGateInvalidFixtureFailsClosed
	// instead, never here.
	{path: "test/fixtures/validate-gate/valid", sourceTree: true},
}

type validatorCommand struct {
	path       string
	prefixArgs []string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/configvalidate <goobers-binary>")
		return 2
	}
	validator, err := filepath.Abs(args[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "validate-configs: resolve validator path: %v\n", err)
		return 2
	}
	if _, err := os.Stat(validator); err != nil {
		_, _ = fmt.Fprintf(stderr, "validate-configs: validator not found at %s: %v\n", validator, err)
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "validate-configs: resolve repository root: %v\n", err)
		return 2
	}
	return validateCheckedInTrees(root, validatorCommand{path: validator}, stdout, stderr)
}

func validateCheckedInTrees(root string, validator validatorCommand, stdout, stderr io.Writer) int {
	return validateTrees(root, checkedInTrees, validator, stdout, stderr)
}

func validateTrees(root string, trees []checkedInTree, validator validatorCommand, stdout, stderr io.Writer) int {
	gitEnv, err := gitWorktreeEnv(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "validate-configs: resolve repository context: %v\n", err)
		return 2
	}

	tempDir, err := os.MkdirTemp("", "goobers-validate-configs-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "validate-configs: create temporary instance roots: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	failed := false
	for _, tree := range trees {
		_, _ = fmt.Fprintf(stdout, "==> validate-config %s\n", tree.path)
		args, err := validationArgs(root, tempDir, tree)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "validate-configs: prepare %s: %v\n", tree.path, err)
			failed = true
			continue
		}
		commandArgs := append(append([]string(nil), validator.prefixArgs...), args...)
		cmd := exec.Command(validator.path, commandArgs...)
		cmd.Dir = root
		cmd.Env = gitEnv
		var commandStdout, commandStderr bytes.Buffer
		if len(tree.allowedWarnings) > 0 {
			cmd.Stdout = &commandStdout
			cmd.Stderr = &commandStderr
		} else {
			cmd.Stdout = stdout
			cmd.Stderr = stderr
		}
		runErr := cmd.Run()
		if len(tree.allowedWarnings) > 0 {
			_, _ = io.WriteString(stdout, commandStdout.String())
			_, _ = io.WriteString(stderr, commandStderr.String())
		}
		if runErr != nil {
			_, _ = fmt.Fprintf(stderr, "validate-configs: %s: %v\n", tree.path, runErr)
			failed = true
			continue
		}
		if len(tree.allowedWarnings) > 0 {
			got := validationWarnings(commandStdout.String())
			if !equalStrings(got, tree.allowedWarnings) {
				_, _ = fmt.Fprintf(
					stderr,
					"validate-configs: %s: warnings changed: got %q, want %q\n",
					tree.path,
					got,
					tree.allowedWarnings,
				)
				failed = true
			}
		}
	}
	if failed {
		return 1
	}
	return 0
}

func gitWorktreeEnv(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "rev-parse", "--absolute-git-dir", "--show-toplevel")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git rev-parse: %w: %s", err, strings.TrimSpace(string(output)))
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(string(output)), "\r\n", "\n"), "\n")
	if len(lines) != 2 {
		return nil, fmt.Errorf("git rev-parse returned %d lines, want 2", len(lines))
	}

	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "GIT_DIR") || strings.EqualFold(key, "GIT_WORK_TREE") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_DIR="+strings.TrimSpace(lines[0]), "GIT_WORK_TREE="+strings.TrimSpace(lines[1])), nil
}

func validationWarnings(output string) []string {
	var warnings []string
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "WARNING ") {
			warnings = append(warnings, line)
		}
	}
	return warnings
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validationArgs(root, tempDir string, tree checkedInTree) ([]string, error) {
	args := []string{"validate"}
	if tree.strict && len(tree.allowedWarnings) == 0 {
		args = append(args, "--strict")
	}
	if tree.sourceTree {
		return append(args, "--source-tree", tree.path), nil
	}

	instanceRoot := filepath.Join(tempDir, strings.NewReplacer("/", "-", `\`, "-").Replace(tree.path))
	configDir := filepath.Join(instanceRoot, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(instanceRoot, "instance.yaml"), []byte(instanceYAML), 0o644); err != nil {
		return nil, err
	}

	source := filepath.Join(root, filepath.FromSlash(tree.path))
	if err := os.CopyFS(configDir, os.DirFS(source)); err != nil {
		return nil, err
	}
	return append(args, instanceRoot), nil
}
