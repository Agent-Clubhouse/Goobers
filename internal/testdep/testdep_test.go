package testdep

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type fakeTB struct {
	helpers int
	fatal   string
	skip    string
}

func (f *fakeTB) Helper() {
	f.helpers++
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
}

func (f *fakeTB) Skipf(format string, args ...any) {
	f.skip = fmt.Sprintf(format, args...)
}

func TestRequireAllowsPresentDependencies(t *testing.T) {
	tb := &fakeTB{}
	require(tb, func(tool string) (string, error) {
		return "/bin/" + tool, nil
	}, false, "sh", "sleep")

	if tb.fatal != "" || tb.skip != "" {
		t.Fatalf("fatal=%q skip=%q, want neither", tb.fatal, tb.skip)
	}
	if tb.helpers == 0 {
		t.Fatal("Require did not mark itself as a test helper")
	}
}

func TestRequireSkipsMissingDependencyWithInstallHint(t *testing.T) {
	tb := &fakeTB{}
	require(tb, missingLookup, false, "sh")

	for _, want := range []string{
		"integration test skipped",
		"sh",
		"apt-get install dash",
	} {
		if !strings.Contains(tb.skip, want) {
			t.Errorf("skip message %q does not contain %q", tb.skip, want)
		}
	}
	if tb.fatal != "" {
		t.Fatalf("fatal=%q, want lenient skip", tb.fatal)
	}
}

func TestRequireFailsMissingDependencyInStrictMode(t *testing.T) {
	tb := &fakeTB{}
	require(tb, missingLookup, true, "bwrap")

	for _, want := range []string{
		StrictEnv + "=1",
		"bwrap",
		"apt-get install bubblewrap",
	} {
		if !strings.Contains(tb.fatal, want) {
			t.Errorf("fatal message %q does not contain %q", tb.fatal, want)
		}
	}
	if tb.skip != "" {
		t.Fatalf("skip=%q, strict mode must not skip", tb.skip)
	}
}

func TestRequireRejectsUnregisteredDependency(t *testing.T) {
	tb := &fakeTB{}
	require(tb, missingLookup, false, "not-declared")

	if !strings.Contains(tb.fatal, `"not-declared" is not declared`) {
		t.Fatalf("fatal=%q, want undeclared-dependency error", tb.fatal)
	}
	if tb.skip != "" {
		t.Fatalf("skip=%q, undeclared dependency must fail", tb.skip)
	}
}

func TestDependenciesAreSorted(t *testing.T) {
	got := Dependencies()
	want := []Dependency{
		{Name: "bash", InstallHint: "install Bash (Debian/Ubuntu: apt-get install bash)"},
		{Name: "bwrap", InstallHint: "install bubblewrap (Debian/Ubuntu: apt-get install bubblewrap)"},
		{Name: "copilot", InstallHint: "install and sign in to the GitHub Copilot CLI (https://docs.github.com/copilot/using-github-copilot/using-github-copilot-in-the-command-line)"},
		{Name: "dirname", InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)"},
		{Name: "dotnet", InstallHint: "install the .NET SDK (https://dotnet.microsoft.com/download)"},
		{Name: "head", InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)"},
		{Name: "mkdir", InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)"},
		{Name: "sh", InstallHint: "install a POSIX shell (Debian/Ubuntu: apt-get install dash)"},
		{Name: "sleep", InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)"},
		{Name: "yes", InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Dependencies() = %#v, want %#v", got, want)
	}
}

func TestRequireEnvSkipsWhenAllUnset(t *testing.T) {
	tb := &fakeTB{}
	RequireEnv(tb, "GOOBERS_TESTDEP_UNSET_A", "GOOBERS_TESTDEP_UNSET_B")

	for _, want := range []string{"GOOBERS_TESTDEP_UNSET_A", "GOOBERS_TESTDEP_UNSET_B", "opt in"} {
		if !strings.Contains(tb.skip, want) {
			t.Errorf("skip message %q does not contain %q", tb.skip, want)
		}
	}
	if tb.fatal != "" {
		t.Fatalf("fatal=%q, want lenient skip", tb.fatal)
	}
}

func TestRequireEnvPassesWhenSet(t *testing.T) {
	t.Setenv("GOOBERS_TESTDEP_SET", "1")
	tb := &fakeTB{}
	RequireEnv(tb, "GOOBERS_TESTDEP_SET")

	if tb.fatal != "" || tb.skip != "" {
		t.Fatalf("fatal=%q skip=%q, want neither", tb.fatal, tb.skip)
	}
	if tb.helpers == 0 {
		t.Fatal("RequireEnv did not mark itself as a test helper")
	}
}

func TestRequireEnvPassesOnFallbackName(t *testing.T) {
	t.Setenv("GOOBERS_TESTDEP_FALLBACK", "value")
	tb := &fakeTB{}
	RequireEnv(tb, "GOOBERS_TESTDEP_PRIMARY_UNSET", "GOOBERS_TESTDEP_FALLBACK")

	if tb.fatal != "" || tb.skip != "" {
		t.Fatalf("fatal=%q skip=%q, want neither", tb.fatal, tb.skip)
	}
}

func missingLookup(string) (string, error) {
	return "", errors.New("not found")
}
