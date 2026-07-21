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
)

func TestValidateCheckedInTreesRunsEveryTree(t *testing.T) {
	t.Setenv("GO_WANT_CONFIGVALIDATE_HELPER", "1")
	var stdout, stderr bytes.Buffer
	code := validateCheckedInTrees(
		moduleRoot(t),
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

func TestValidateCheckedInTreesFailsOnValidationError(t *testing.T) {
	root := moduleRoot(t)
	var stdout, stderr bytes.Buffer
	code := validateTrees(
		root,
		[]checkedInTree{{path: "test/configvalidate/testdata/invalid"}},
		validatorCommand{path: buildValidator(t, root)},
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("validateCheckedInTrees code=%d, want 1; stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	want := `gaggles/example/workflows/default-implement.yaml Workflow/default-implement: spec.gaggle names "ghost", but no Gaggle/ghost definition was found`
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("validator diagnostic was not preserved:\n%s", &stdout)
	}
	if !strings.Contains(stderr.String(), "test/configvalidate/testdata/invalid") {
		t.Fatalf("failure did not identify the offending config tree:\n%s", &stderr)
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
	sourceTreeValidation := len(args) >= 3 && args[1] == "--source-tree"
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
	_, _ = fmt.Fprintf(os.Stdout, "VALIDATED %s\n", target)
	os.Exit(0)
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
