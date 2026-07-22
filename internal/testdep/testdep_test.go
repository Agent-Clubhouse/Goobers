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
		{Name: "bwrap", InstallHint: "install bubblewrap (Debian/Ubuntu: apt-get install bubblewrap)"},
		{Name: "sh", InstallHint: "install a POSIX shell (Debian/Ubuntu: apt-get install dash)"},
		{Name: "sleep", InstallHint: "install coreutils (Debian/Ubuntu: apt-get install coreutils)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Dependencies() = %#v, want %#v", got, want)
	}
}

func missingLookup(string) (string, error) {
	return "", errors.New("not found")
}
