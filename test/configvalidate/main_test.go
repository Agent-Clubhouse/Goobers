package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

func TestValidateCheckedInTreesRunsEveryTreeWithoutPollutingRepository(t *testing.T) {
	t.Setenv("GO_WANT_CONFIGVALIDATE_HELPER", "1")
	root := t.TempDir()
	for _, tree := range checkedInTrees {
		source := filepath.Join(root, filepath.FromSlash(tree.path))
		if err := os.MkdirAll(source, 0o755); err != nil {
			t.Fatal(err)
		}
		if !tree.sourceTree {
			if err := os.WriteFile(filepath.Join(source, "manifest.yaml"), []byte("fixture"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	script := filepath.Join(root, "config-examples", "scripts", "check-todos.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepository(t, root)
	t.Setenv("GO_CONFIGVALIDATE_SCAN_ROOT", root)

	var stdout, stderr bytes.Buffer
	code := validateCheckedInTrees(
		root,
		validatorCommand{
			path:       os.Args[0],
			prefixArgs: []string{"-test.run=TestValidatorHelperProcess", "--"},
		},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("validateCheckedInTrees code=%d, want 0; stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	if got := strings.Count(stdout.String(), "VALIDATED "); got != len(checkedInTrees) {
		t.Fatalf("validator calls=%d, want %d; output:\n%s", got, len(checkedInTrees), &stdout)
	}
}

func TestValidateCheckedInTreesFailsOnMissingDocsRoot(t *testing.T) {
	module := moduleRoot(t)
	root := t.TempDir()
	source := filepath.Join(root, "config-under-test")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(module, "test", "configvalidate", "testdata", "invalid")
	if err := os.CopyFS(source, os.DirFS(fixture)); err != nil {
		t.Fatal(err)
	}
	initGitRepository(t, root)
	// #3285: the docs-root existence ERROR only fires when the validated tree
	// is a checkout of the fixture gaggle's target repository (spec.project =
	// example/example); without this remote the finding is an advisory
	// warning and the gate would not exercise the fail-closed path.
	addGitRemote(t, root, "https://github.com/example/example.git")

	var stdout, stderr bytes.Buffer
	code := validateTrees(
		root,
		[]checkedInTree{{path: "config-under-test"}},
		validatorCommand{path: buildValidator(t, module)},
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("validateCheckedInTrees code=%d, want 1; stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	want := `DOCSROOTS Workflow/default-implement: declared docs root "missing-docs-root" does not exist`
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("validator diagnostic was not preserved:\n%s", &stdout)
	}
	if strings.Contains(stdout.String(), "skipped existence check") {
		t.Fatalf("repository-backed docs-root check was skipped:\n%s", &stdout)
	}
	if !strings.Contains(stderr.String(), "config-under-test") {
		t.Fatalf("failure did not identify the offending config tree:\n%s", &stderr)
	}
}

// TestValidateGateInvalidFixtureFailsClosed is #687's deliberately-failing
// fixture (test/fixtures/validate-gate/invalid): the config-repo PR gate's
// self-test workflow (.github/actions/validate) exercises this fixture
// expecting a non-zero exit, and this test pins that expectation at the Go
// level so a change that accidentally makes the fixture valid — or breaks it
// in some OTHER way than the intended one — is caught here rather than only
// surfacing as a confusing CI-workflow failure.
func TestValidateGateInvalidFixtureFailsClosed(t *testing.T) {
	module := moduleRoot(t)
	root := t.TempDir()
	source := filepath.Join(root, "config-under-test")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(module, "test", "fixtures", "validate-gate", "invalid")
	if err := os.CopyFS(source, os.DirFS(fixture)); err != nil {
		t.Fatal(err)
	}
	initGitRepository(t, root)

	var stdout, stderr bytes.Buffer
	code := validateTrees(
		root,
		[]checkedInTree{{path: "config-under-test", sourceTree: true}},
		validatorCommand{path: buildValidator(t, module)},
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("validateTrees code=%d, want 1; stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	want := `spec.gaggle names "ghost", but no Gaggle/ghost definition was found`
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("validator diagnostic was not preserved:\n%s", &stdout)
	}
}

func TestValidateTreesRejectsWarningDrift(t *testing.T) {
	t.Setenv("GO_WANT_CONFIGVALIDATE_HELPER", "1")
	t.Setenv("GO_CONFIGVALIDATE_WARNING", "WARNING Workflow/other: unexpected warning")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "config-under-test"), 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepository(t, root)

	var stdout, stderr bytes.Buffer
	code := validateTrees(
		root,
		[]checkedInTree{{
			path:            "config-under-test",
			sourceTree:      true,
			strict:          true,
			allowedWarnings: []string{"WARNING Workflow/docs-updater: expected warning"},
		}},
		validatorCommand{
			path:       os.Args[0],
			prefixArgs: []string{"-test.run=TestValidatorHelperProcess", "--"},
		},
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("validateTrees code=%d, want 1; stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	if !strings.Contains(stderr.String(), "warnings changed") {
		t.Fatalf("warning drift was not reported: %q", &stderr)
	}
}

func TestValidationArgsKeepsStrictWithoutAllowlist(t *testing.T) {
	args, err := validationArgs(t.TempDir(), t.TempDir(), checkedInTree{
		path:       "strict-source",
		sourceTree: true,
		strict:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgument(args, "--strict") {
		t.Fatalf("validation args = %q, want --strict", args)
	}
}

func TestRunRejectsMissingValidator(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{filepath.Join(t.TempDir(), "missing")}, &stdout, &stderr); code != 2 {
		t.Fatalf("run code=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "validator not found") {
		t.Fatalf("stderr missing validator error: %q", &stderr)
	}
}

func TestValidatorHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CONFIGVALIDATE_HELPER") != "1" {
		return
	}
	args := argsAfterSeparator(os.Args)
	if len(args) < 2 || args[0] != "validate" {
		_, _ = fmt.Fprintf(os.Stderr, "unexpected validator arguments: %q\n", args)
		os.Exit(2)
	}
	target := args[len(args)-1]
	if containsArgument(args, "--strict") {
		_, _ = fmt.Fprintf(os.Stderr, "validator strictness does not match target: %q\n", args)
		os.Exit(2)
	}
	sourceTreeValidation := containsArgument(args, "--source-tree")
	if !sourceTreeValidation {
		for _, path := range []string{
			filepath.Join(target, "instance.yaml"),
			filepath.Join(target, "config", "manifest.yaml"),
		} {
			if _, err := os.Stat(path); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "prepared config missing %s: %v\n", path, err)
				os.Exit(2)
			}
		}
	}
	if root := os.Getenv("GO_CONFIGVALIDATE_SCAN_ROOT"); root != "" {
		if err := scanToolchainShellScripts(root); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if warning := os.Getenv("GO_CONFIGVALIDATE_WARNING"); warning != "" {
		_, _ = fmt.Fprintln(os.Stdout, warning)
	} else if filepath.Base(target) == "reference-workflows" {
		for _, warning := range checkedInTrees[0].allowedWarnings {
			_, _ = fmt.Fprintln(os.Stdout, warning)
		}
	}
	_, _ = fmt.Fprintf(os.Stdout, "VALIDATED %s\n", target)
	os.Exit(0)
}

func containsArgument(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func scanToolchainShellScripts(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() && (rel == ".git" || rel == "config-examples") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sh") {
			return fmt.Errorf("shell script copied onto toolchain path: %s", rel)
		}
		return nil
	})
}

func argsAfterSeparator(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}
	return nil
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func initGitRepository(t *testing.T, root string) {
	t.Helper()
	cmd := testgit.Command("init", "-q", root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture repository: %v\n%s", err, output)
	}
}

// addGitRemote points the fixture repository's origin at url so the validator
// treats the fixture tree as a checkout of the gaggle's target repository
// (#3285's docsRoots existence-check precondition).
func addGitRemote(t *testing.T, root, url string) {
	t.Helper()
	cmd := testgit.Command("-C", root, "remote", "add", "origin", url)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add fixture remote: %v\n%s", err, output)
	}
}

func buildValidator(t *testing.T, root string) string {
	t.Helper()
	name := "goobers"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/goobers")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build validator: %v\n%s", err, output)
	}
	return binary
}
