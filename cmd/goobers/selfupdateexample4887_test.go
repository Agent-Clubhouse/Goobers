package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	hashiversion "github.com/hashicorp/go-version"

	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/version"
)

// The shipped example must be one the ordering guard can actually accept
// (#4887). It previously pinned v0.1.0, so the documented command failed
// verbatim for every reader who ran it against any build since.
//
// This asserts against the SAME comparison self-update performs rather than
// against a hardcoded expectation, so the example cannot rot back into being
// unrunnable without this failing.
func TestShippedManualExampleIsNewerThanTheRunningBuild(t *testing.T) {
	example := manualSelfUpdateExample(t)
	current := version.Get().Version
	if current == "dev" {
		// A dev build has no comparable version, which is the state of every
		// plain `go build`. Compare against the release the repository plans
		// to cut instead, which is what a reader of these docs will be on.
		current = nextPlannedReleaseForExample(t)
	}
	newer, err := selfUpdateExampleIsNewer(current, example)
	if err != nil {
		t.Fatalf("compare example target %q with %q: %v", example, current, err)
	}
	if !newer {
		t.Errorf("shipped example targets %s, which is not strictly newer than %s — "+
			"running the documented command verbatim would be refused by the ordering guard",
			example, current)
	}
}

// The example is declared once in the command registry and rendered into the
// man page and CLI reference, so all three move together.
func TestManualExampleMatchesTheGeneratedDocs(t *testing.T) {
	example := manualSelfUpdateExample(t)
	for _, path := range []string{"../../docs/man/goobers-self-update.1", "../../docs/cli/README.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(data), "--policy manual --target "+example) {
			t.Errorf("%s does not carry the registry's manual example target %q; run `make docs`", path, example)
		}
	}
}

func manualSelfUpdateExample(t *testing.T) string {
	t.Helper()
	for _, example := range selfUpdateExamples(t) {
		if !strings.Contains(example, "--policy manual") {
			continue
		}
		match := regexp.MustCompile(`--target (\S+)`).FindStringSubmatch(example)
		if match == nil {
			t.Fatalf("manual example %q carries no --target", example)
		}
		return match[1]
	}
	t.Fatal("self-update declares no manual example")
	return ""
}

// selfUpdateExamples reads the examples straight off the command registry,
// which is the single source the man page and CLI reference are generated
// from.
func selfUpdateExamples(t *testing.T) []string {
	t.Helper()
	for _, c := range cliCommands {
		if docDisplayName(c) == "self-update" {
			return c.examples
		}
	}
	t.Fatal("self-update is not registered")
	return nil
}

// selfUpdateExampleIsNewer applies the same strictly-newer comparison
// self-update itself performs, so this test cannot drift from the guard it
// is asserting about.
func selfUpdateExampleIsNewer(current, target string) (bool, error) {
	currentVersion, err := hashiversion.NewVersion(current)
	if err != nil {
		return false, err
	}
	targetVersion, err := hashiversion.NewVersion(target)
	if err != nil {
		return false, err
	}
	return targetVersion.GreaterThan(currentVersion), nil
}

func nextPlannedReleaseForExample(t *testing.T) string {
	t.Helper()
	return supportmatrix.NextPlannedRelease
}
