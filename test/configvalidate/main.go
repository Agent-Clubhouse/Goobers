// Command configvalidate runs the built validator over every checked-in config.
package main

import (
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
    owner: example
    name: example
    token:
      env: GOOBERS_GITHUB_TOKEN
`

type checkedInTree struct {
	path       string
	sourceTree bool
}

var checkedInTrees = []checkedInTree{
	{path: "selfhost", sourceTree: true},
	{path: "config-examples"},
	{path: "internal/instance/starter"},
	{path: "internal/instance/demo"},
	{path: "test/fixtures/e2e/walking-skeleton"},
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
	tempDir, err := os.MkdirTemp(root, ".validate-configs-")
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
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			_, _ = fmt.Fprintf(stderr, "validate-configs: %s: %v\n", tree.path, err)
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}

func validationArgs(root, tempDir string, tree checkedInTree) ([]string, error) {
	if tree.sourceTree {
		return []string{"validate", "--source-tree", tree.path}, nil
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
	return []string{"validate", instanceRoot}, nil
}
