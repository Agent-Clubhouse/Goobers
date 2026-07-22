// Package testdep declares and checks external tools used by integration tests.
package testdep

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// StrictEnv makes a missing integration-test dependency fail instead of skip.
const StrictEnv = "TESTDEP_STRICT"

// Dependency describes one external executable and how to install it.
type Dependency struct {
	Name        string
	InstallHint string
}

var declared = map[string]Dependency{
	"bash": {
		Name:        "bash",
		InstallHint: "install Bash (Debian/Ubuntu: apt-get install bash)",
	},
	"bwrap": {
		Name:        "bwrap",
		InstallHint: "install bubblewrap (Debian/Ubuntu: apt-get install bubblewrap)",
	},
	"dirname": {
		Name:        "dirname",
		InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)",
	},
	"head": {
		Name:        "head",
		InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)",
	},
	"mkdir": {
		Name:        "mkdir",
		InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)",
	},
	"sh": {
		Name:        "sh",
		InstallHint: "install a POSIX shell (Debian/Ubuntu: apt-get install dash)",
	},
	"sleep": {
		Name:        "sleep",
		InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)",
	},
	"yes": {
		Name:        "yes",
		InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)",
	},
}

// TB is the testing surface needed by Require.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// Dependencies returns the declared dependency inventory in name order.
func Dependencies() []Dependency {
	result := make([]Dependency, 0, len(declared))
	for _, dependency := range declared {
		result = append(result, dependency)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// Require verifies that each declared executable is available on PATH.
func Require(t TB, tools ...string) {
	t.Helper()
	require(t, exec.LookPath, os.Getenv(StrictEnv) == "1", tools...)
}

type lookupFunc func(string) (string, error)

func require(t TB, lookup lookupFunc, strict bool, tools ...string) {
	t.Helper()

	seen := make(map[string]bool, len(tools))
	missing := make([]Dependency, 0, len(tools))
	for _, tool := range tools {
		dependency, ok := declared[tool]
		if !ok {
			t.Fatalf("integration test dependency %q is not declared in testdep inventory", tool)
			return
		}
		if seen[tool] {
			continue
		}
		seen[tool] = true
		if _, err := lookup(tool); err != nil {
			missing = append(missing, dependency)
		}
	}
	if len(missing) == 0 {
		return
	}

	details := make([]string, 0, len(missing))
	for _, dependency := range missing {
		details = append(details, fmt.Sprintf("%s (%s)", dependency.Name, dependency.InstallHint))
	}
	message := strings.Join(details, "; ")
	if strict {
		t.Fatalf("missing declared integration test dependency with %s=1: %s", StrictEnv, message)
		return
	}
	t.Skipf("integration test skipped: missing declared dependency: %s", message)
}
