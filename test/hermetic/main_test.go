package main

import (
	"bytes"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestHermeticEnvironmentReplacesAmbientToolAndNetworkSettings(t *testing.T) {
	got := hermeticEnvironment([]string{
		"HOME=/home/tester",
		"PATH=/ambient/bin",
		"GOPROXY=https://proxy.example",
		"GOPRIVATE=example.com",
		"GOFLAGS=-mod=mod",
		"CC=ambient-cc",
	}, "/isolated/tools", "hermetic-cc")

	values := environmentMap(got)
	for name, want := range map[string]string{
		"CC":          "hermetic-cc",
		"GOENV":       "off",
		"GOFLAGS":     "-mod=readonly",
		"GONOPROXY":   "none",
		"GONOSUMDB":   "none",
		"GOPRIVATE":   "",
		"GOPROXY":     "off",
		"GOSUMDB":     "off",
		"GOTOOLCHAIN": "local",
		"GOVCS":       "*:off",
		"HOME":        "/home/tester",
		"PATH":        "/isolated/tools",
	} {
		if values[name] != want {
			t.Errorf("%s = %q, want %q", name, values[name], want)
		}
	}
}

func TestAuditTestExecsRejectsNonAllowlistedTools(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "go.mod"), "module example.com/hermetic\n\ngo 1.26\n")
	writeFixture(t, filepath.Join(root, "unit_test.go"), `package fixture

import (
	"context"
	command "os/exec"
)

func unit() {
	_ = command.Command("copilot", "--version")
	_ = command.CommandContext(context.Background(), "docker", "version")
	_ = command.Command("git", "status")
	_ = command.Command("./fixture-tool")
}
`)
	writeFixture(t, filepath.Join(root, "live_test.go"), `//go:build integration

package fixture

import "os/exec"

func live() {
	_ = exec.Command("copilot")
}
`)

	got, err := auditTestExecs(root, map[string]struct{}{"git": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("violations = %#v, want copilot and docker", got)
	}
	if got[0].tool != "copilot" || got[1].tool != "docker" {
		t.Fatalf("tools = %q, %q, want copilot, docker", got[0].tool, got[1].tool)
	}
	for _, item := range got {
		if item.position.Filename != "unit_test.go" {
			t.Errorf("position filename = %q, want unit_test.go", item.position.Filename)
		}
	}
}

func TestReportViolationsDirectsAuthorToIntegrationTier(t *testing.T) {
	var output bytes.Buffer
	reportViolations(&output, []violation{{
		position: token.Position{Filename: "fixture_test.go", Line: 12, Column: 3},
		tool:     "copilot",
	}})
	for _, want := range []string{"fixture_test.go:12:3", "copilot not allowlisted", "//go:build integration", "integration tier"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("diagnostic %q does not contain %q", output.String(), want)
		}
	}
}

func TestPopulateToolPathContainsOnlyResolvedTools(t *testing.T) {
	sourceDir := t.TempDir()
	first := filepath.Join(sourceDir, executableName("go"))
	second := filepath.Join(sourceDir, executableName("git"))
	writeFixture(t, first, "go")
	writeFixture(t, second, "git")

	destination := t.TempDir()
	if err := populateToolPath(destination, []resolvedTool{
		{name: "go", path: first},
		{name: "git", path: second},
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{executableName("git"), executableName("go")}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tool PATH entries = %q, want %q", names, want)
	}
	if runtime.GOOS != "windows" {
		for _, entry := range entries {
			info, err := os.Lstat(filepath.Join(destination, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s is not a symlink", entry.Name())
			}
		}
	}
}

func TestMissingExecToolDiagnostics(t *testing.T) {
	tests := map[string]string{
		`exec: "copilot": executable file not found in $PATH`: "copilot",
		"bash: docker: command not found":                     "docker",
		"sh: 1: gh: not found":                                "gh",
		`exec: "git": permission denied`:                      "",
	}
	for line, want := range tests {
		if got := missingExecTool(line); got != want {
			t.Errorf("missingExecTool(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestDiagnosticCollectorReportsOnlyNonAllowlistedTools(t *testing.T) {
	collector := &diagnosticCollector{
		allowed: map[string]struct{}{"git": {}},
		tools:   make(map[string]struct{}),
	}
	var output bytes.Buffer
	writer := &diagnosticWriter{destination: &output, collector: collector}
	_, _ = writer.Write([]byte("exec: \"git\": executable file not found in $PATH\n"))
	_, _ = writer.Write([]byte("exec: \"copilot\": executable file not found in $PATH\n"))
	writer.flush()

	if got := collector.missingTools(); !reflect.DeepEqual(got, []string{"copilot"}) {
		t.Fatalf("missing tools = %q, want [copilot]", got)
	}
}

func TestRunRequiresGoTestArguments(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(nil, &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("run exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, variable := range environment {
		name := environmentName(variable)
		result[name] = strings.TrimPrefix(variable, name+"=")
	}
	return result
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
