package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHasIntegrationTag(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want bool
	}{
		{name: "integration", tag: "//go:build integration", want: true},
		{name: "combined", tag: "//go:build linux && integration", want: true},
		{name: "negated", tag: "//go:build !integration", want: false},
		{name: "other", tag: "//go:build linux", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := hasIntegrationTag([]byte(test.tag + "\n\npackage fixture\n"))
			if err != nil {
				t.Fatalf("hasIntegrationTag: %v", err)
			}
			if got != test.want {
				t.Fatalf("hasIntegrationTag(%q) = %v, want %v", test.tag, got, test.want)
			}
		})
	}
}

func TestScanIntegrationDiscoversPackagesAndDependencies(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "alpha/regular_test.go", "package alpha\n")
	writeFixture(t, root, "alpha/tool_integration_test.go", `//go:build integration

package alpha

import (
	"testing"

	"github.com/goobers/goobers/internal/testdep"
)

func TestIntegrationTool(t *testing.T) {
	testdep.Require(t, "sh", "sleep")
}
`)
	writeFixture(t, root, "beta/tool_integration_test.go", `//go:build linux && integration

package beta

import (
	"testing"

	deps "github.com/goobers/goobers/internal/testdep"
)

func TestIntegrationTool(t *testing.T) {
	deps.Require(t, "bwrap")
}
`)
	writeFixture(t, root, "gamma/tool_live_test.go", `//go:build integration

package gamma

import "testing"

func TestToolLive(t *testing.T) {
	t.Skip("opt-in live test")
}
`)

	got, err := scanIntegration(root)
	if err != nil {
		t.Fatalf("scanIntegration: %v", err)
	}
	if want := []string{"./alpha", "./beta"}; !reflect.DeepEqual(got.packages, want) {
		t.Fatalf("packages = %q, want %q", got.packages, want)
	}
	if want := map[string]bool{"bwrap": true, "sh": true, "sleep": true}; !reflect.DeepEqual(got.dependencies, want) {
		t.Fatalf("dependencies = %v, want %v", got.dependencies, want)
	}
}

func TestScanIntegrationDoesNotInferLiveTierFromMissingDeclarations(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "broken/tool_integration_test.go", `//go:build integration

package broken

import "testing"

func TestTool(t *testing.T) {
	t.Log("missing declared-dependency contract")
}
`)

	_, err := scanIntegration(root)
	if err == nil {
		t.Fatal("scanIntegration accepted malformed declared-dependency test")
	}
	for _, want := range []string{
		"integration tests must use the TestIntegration prefix",
		"integration-tagged file has no testdep.Require declaration",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestIntegrationDependencyGuard(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		wantError string
	}{
		{
			name: "direct lookup",
			source: `package fixture
import (
	"os/exec"
	"testing"
	"github.com/goobers/goobers/internal/testdep"
)
func TestIntegrationTool(t *testing.T) {
	testdep.Require(t, "sh")
	_, _ = exec.LookPath("sh")
}`,
			wantError: "direct exec.LookPath is forbidden",
		},
		{
			name: "raw skip",
			source: `package fixture
import (
	"testing"
	"github.com/goobers/goobers/internal/testdep"
)
func TestIntegrationTool(t *testing.T) {
	testdep.Require(t, "sh")
	t.Skip("missing")
}`,
			wantError: "raw test skip is forbidden",
		},
		{
			name: "raw skip now",
			source: `package fixture
import (
	"testing"
	"github.com/goobers/goobers/internal/testdep"
)
func TestIntegrationTool(t *testing.T) {
	testdep.Require(t, "sh")
	t.SkipNow()
}`,
			wantError: "raw test skip is forbidden",
		},
		{
			name: "missing declaration",
			source: `package fixture
import "testing"
func TestIntegrationTool(t *testing.T) { t.Log("no external dependency") }`,
			wantError: "has no testdep.Require or testdep.RequireEnv declaration",
		},
		{
			name: "dynamic declaration",
			source: `package fixture
import (
	"testing"
	"github.com/goobers/goobers/internal/testdep"
)
func TestIntegrationTool(t *testing.T) {
	tool := "sh"
	testdep.Require(t, tool)
}`,
			wantError: "dependencies must be string literals",
		},
		{
			name: "late declaration",
			source: `package fixture
import (
	"testing"
	"github.com/goobers/goobers/internal/testdep"
)
func TestIntegrationTool(t *testing.T) {
	t.Log("setup")
	testdep.Require(t, "sh")
}`,
			wantError: "must call testdep.Require or testdep.RequireEnv as their first statement",
		},
		{
			name: "env gate names nothing",
			source: `package fixture
import (
	"testing"
	"github.com/goobers/goobers/internal/testdep"
)
func TestIntegrationTool(t *testing.T) {
	testdep.RequireEnv(t)
}`,
			wantError: "testdep.RequireEnv must name at least one variable",
		},
		{
			name: "wrong test prefix",
			source: `package fixture
import (
	"testing"
	"github.com/goobers/goobers/internal/testdep"
)
func TestTool(t *testing.T) {
	testdep.Require(t, "sh")
}`,
			wantError: "must use the TestIntegration prefix",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, violations, err := inspectIntegrationFile("fixture_test.go", []byte(test.source))
			if err != nil {
				t.Fatalf("inspectIntegrationFile: %v", err)
			}
			if got := strings.Join(violations, "\n"); !strings.Contains(got, test.wantError) {
				t.Fatalf("violations = %q, want error containing %q", got, test.wantError)
			}
		})
	}
}

func TestIntegrationDependencyGuardAllowsRequireEnvOnlyFile(t *testing.T) {
	source := `package fixture
import (
	"testing"
	"github.com/goobers/goobers/internal/testdep"
)
func TestIntegrationTool(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_LIVE_SMOKE")
}`
	dependencies, violations, err := inspectIntegrationFile("fixture_test.go", []byte(source))
	if err != nil {
		t.Fatalf("inspectIntegrationFile: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none — RequireEnv alone must satisfy the first-statement + declaration checks", violations)
	}
	if len(dependencies) != 0 {
		t.Fatalf("dependencies = %v, want none — RequireEnv names are environment variables, not tool dependencies", dependencies)
	}
}

func TestValidateInventory(t *testing.T) {
	if err := validateInventory(map[string]bool{
		"bash": true, "bwrap": true, "copilot": true, "dirname": true, "dotnet": true,
		"head": true, "mkdir": true, "sh": true, "sleep": true, "yes": true,
	}); err != nil {
		t.Fatalf("validateInventory exact match: %v", err)
	}

	err := validateInventory(map[string]bool{"sh": true, "unknown": true})
	if err == nil {
		t.Fatal("validateInventory drift succeeded")
	}
	for _, want := range []string{
		`dependency "unknown" is required`,
		`inventory dependency "bash" is not required`,
		`inventory dependency "bwrap" is not required`,
		`inventory dependency "copilot" is not required`,
		`inventory dependency "dirname" is not required`,
		`inventory dependency "dotnet" is not required`,
		`inventory dependency "head" is not required`,
		`inventory dependency "mkdir" is not required`,
		`inventory dependency "sleep" is not required`,
		`inventory dependency "yes" is not required`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	filePath := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
