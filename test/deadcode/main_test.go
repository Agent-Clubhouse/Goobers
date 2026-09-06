package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseExemptionsRequiresExactSymbolAndReason(t *testing.T) {
	t.Parallel()
	got, err := parseExemptions(strings.NewReader(`
# Reviewed compatibility surfaces.
github.com/goobers/goobers/api/v1alpha1.Resource # Kubernetes API helper retained for consumers.
github.com/goobers/goobers/internal/testdep.Require # Called only from integration-tagged tests.
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]exemption{
		"github.com/goobers/goobers/api/v1alpha1.Resource":    {reason: "Kubernetes API helper retained for consumers."},
		"github.com/goobers/goobers/internal/testdep.Require": {reason: "Called only from integration-tagged tests."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exemptions = %#v, want %#v", got, want)
	}

	for _, input := range []string{
		"github.com/goobers/goobers/internal/testdep.Require",
		"github.com/goobers/goobers/internal/testdep.Require # ",
		"github.com/goobers/goobers/internal/testdep.Require # reason\n" +
			"github.com/goobers/goobers/internal/testdep.Require # duplicate",
	} {
		if _, err := parseExemptions(strings.NewReader(input)); err == nil {
			t.Errorf("parseExemptions(%q) succeeded, want error", input)
		}
	}
}

// TestParseExemptionsPlatformQualifier is #4434's evidence for the new
// "[platform,...] symbol # reason" syntax: a qualified entry parses into its
// declared platform subset, an unqualified one stays nil (all platforms),
// and every malformed or unsupported qualifier shape is rejected.
func TestParseExemptionsPlatformQualifier(t *testing.T) {
	t.Parallel()
	got, err := parseExemptions(strings.NewReader(
		"[windows] github.com/goobers/goobers/internal/sandbox.resolveDirectory # Unix-only native sandbox.\n" +
			"[linux,darwin] github.com/goobers/goobers/internal/platform/shutdown.NewExternal # Windows-only service caller.\n" +
			"github.com/goobers/goobers/internal/testdep.Require # Platform-neutral.\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]exemption{
		"github.com/goobers/goobers/internal/sandbox.resolveDirectory":      {platforms: []string{"windows"}, reason: "Unix-only native sandbox."},
		"github.com/goobers/goobers/internal/platform/shutdown.NewExternal": {platforms: []string{"linux", "darwin"}, reason: "Windows-only service caller."},
		"github.com/goobers/goobers/internal/testdep.Require":               {reason: "Platform-neutral."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exemptions = %#v, want %#v", got, want)
	}

	for _, input := range []string{
		"[windows github.com/goobers/goobers/internal/testdep.Require # unterminated qualifier",
		"[] github.com/goobers/goobers/internal/testdep.Require # empty qualifier",
		"[,] github.com/goobers/goobers/internal/testdep.Require # empty platform in list",
		"[plan9] github.com/goobers/goobers/internal/testdep.Require # unsupported platform",
	} {
		if _, err := parseExemptions(strings.NewReader(input)); err == nil {
			t.Errorf("parseExemptions(%q) succeeded, want error", input)
		}
	}
}

func TestExemptionProblemsRejectsNewAndStaleEntries(t *testing.T) {
	t.Parallel()
	reports := allPlatformsReport([]reportPackage{{
		Path: "github.com/goobers/goobers/internal/example",
		Funcs: []reportFunction{
			{Name: "Reviewed", Position: reportPosition{File: "internal/example/example.go", Line: 10, Col: 6}},
			{Name: "NewFinding", Position: reportPosition{File: "internal/example/example.go", Line: 20, Col: 6}},
		},
	}})
	exemptions := map[string]exemption{
		"github.com/goobers/goobers/internal/example.Reviewed": {reason: "Intentional extension seam."},
		"github.com/goobers/goobers/internal/example.Removed":  {reason: "Old extension seam."},
	}

	got := exemptionProblems(reports, exemptions)
	want := []string{
		"internal/example/example.go:20:6: unreviewed dead code: github.com/goobers/goobers/internal/example.NewFinding",
		"stale deadcode exemption: github.com/goobers/goobers/internal/example.Removed (Old extension seam.)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("problems = %q, want %q", got, want)
	}
}

// TestExemptionProblemsWindowsOnlySymbol is #4434's core regression: a symbol
// dead ONLY on windows (like internal/sandbox.resolveDirectory/validate, kept
// alive on linux/darwin by the native unix wrapper) must not be reported
// "unreviewed" on windows once a [windows] exemption covers it, and must not
// make that SAME exemption look "stale" on linux/darwin where the symbol is
// correctly reachable and therefore never appears in the report at all.
func TestExemptionProblemsWindowsOnlySymbol(t *testing.T) {
	t.Parallel()
	symbol := "github.com/goobers/goobers/internal/sandbox.resolveDirectory"
	reports := map[string][]reportPackage{
		"linux":   nil,
		"darwin":  nil,
		"windows": {{Path: "github.com/goobers/goobers/internal/sandbox", Funcs: []reportFunction{{Name: "resolveDirectory", Position: reportPosition{File: "internal/sandbox/sandbox.go", Line: 100, Col: 6}}}}},
	}
	exemptions := map[string]exemption{symbol: {platforms: []string{"windows"}, reason: "Unix-only native sandbox."}}

	if got := exemptionProblems(reports, exemptions); len(got) != 0 {
		t.Fatalf("problems = %v, want none — the windows-only dead symbol is fully covered by its [windows] exemption", got)
	}
}

// TestExemptionProblemsUnixOnlySymbol is the mirror case: a symbol dead ONLY
// on linux/darwin (like internal/platform/shutdown.NewExternal, kept alive on
// windows by winsvc_windows.go) must be covered by a [linux,darwin] entry
// without that entry looking stale on windows, where the symbol is correctly
// reachable and never appears in the report.
func TestExemptionProblemsUnixOnlySymbol(t *testing.T) {
	t.Parallel()
	symbol := "github.com/goobers/goobers/internal/platform/shutdown.NewExternal"
	deadReport := []reportPackage{{Path: "github.com/goobers/goobers/internal/platform/shutdown", Funcs: []reportFunction{{Name: "NewExternal", Position: reportPosition{File: "internal/platform/shutdown/shutdown.go", Line: 101, Col: 6}}}}}
	reports := map[string][]reportPackage{
		"linux":   deadReport,
		"darwin":  deadReport,
		"windows": nil,
	}
	exemptions := map[string]exemption{symbol: {platforms: []string{"linux", "darwin"}, reason: "Windows-only service caller."}}

	if got := exemptionProblems(reports, exemptions); len(got) != 0 {
		t.Fatalf("problems = %v, want none — the unix-only dead symbol is fully covered by its [linux,darwin] exemption", got)
	}
}

// TestExemptionProblemsSharedSymbolStillWorks proves an ordinary
// platform-neutral symbol — dead on every target — is unaffected by the
// per-platform machinery: one unqualified exemption still covers it exactly
// as before #4434.
func TestExemptionProblemsSharedSymbolStillWorks(t *testing.T) {
	t.Parallel()
	reports := allPlatformsReport([]reportPackage{{
		Path:  "github.com/goobers/goobers/internal/example",
		Funcs: []reportFunction{{Name: "Shared", Position: reportPosition{File: "internal/example/example.go", Line: 5, Col: 6}}},
	}})
	exemptions := map[string]exemption{"github.com/goobers/goobers/internal/example.Shared": {reason: "Platform-neutral extension seam."}}

	if got := exemptionProblems(reports, exemptions); len(got) != 0 {
		t.Fatalf("problems = %v, want none", got)
	}
}

// TestExemptionProblemsFlagsPlatformSpecificExemptionAsStale proves a
// [windows]-qualified exemption for a symbol that is NOT actually dead on
// windows (only on an unlisted platform, or on none at all) is correctly
// reported stale — the qualifier narrows what counts as covering it, it does
// not exempt it from staleness detection altogether.
func TestExemptionProblemsFlagsPlatformSpecificExemptionAsStale(t *testing.T) {
	t.Parallel()
	symbol := "github.com/goobers/goobers/internal/example.NoLongerDead"
	// Reachable (not reported dead) on every platform now, but a stale
	// [windows] exemption remains for it.
	reports := map[string][]reportPackage{"linux": nil, "darwin": nil, "windows": nil}
	exemptions := map[string]exemption{symbol: {platforms: []string{"windows"}, reason: "Was windows-only."}}

	got := exemptionProblems(reports, exemptions)
	want := []string{"stale deadcode exemption: github.com/goobers/goobers/internal/example.NoLongerDead (Was windows-only.)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("problems = %q, want %q", got, want)
	}
}

// TestExemptionProblemsFlagsUncoveredPlatformOnQualifiedEntry proves a
// [windows]-qualified exemption does NOT silently cover the same symbol
// turning up dead on linux too — that's a NEW, unreviewed finding on a
// platform the entry never declared.
func TestExemptionProblemsFlagsUncoveredPlatformOnQualifiedEntry(t *testing.T) {
	t.Parallel()
	symbol := "github.com/goobers/goobers/internal/example.NewlyDeadOnLinux"
	windowsReport := []reportPackage{{Path: "github.com/goobers/goobers/internal/example", Funcs: []reportFunction{{Name: "NewlyDeadOnLinux", Position: reportPosition{File: "internal/example/example.go", Line: 30, Col: 6}}}}}
	reports := map[string][]reportPackage{
		"linux":   windowsReport,
		"darwin":  nil,
		"windows": windowsReport,
	}
	exemptions := map[string]exemption{symbol: {platforms: []string{"windows"}, reason: "Was expected windows-only."}}

	got := exemptionProblems(reports, exemptions)
	want := []string{"internal/example/example.go:30:6: unreviewed dead code: github.com/goobers/goobers/internal/example.NewlyDeadOnLinux [platforms: linux]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("problems = %q, want %q", got, want)
	}
}

func TestDecodeReportsMatchesDeadcodeSchema(t *testing.T) {
	t.Parallel()
	reports, err := decodeReports([]byte(`[
		{"Path":"github.com/goobers/goobers/internal/example","Funcs":[
			{"Name":"Unused","Position":{"File":"example.go","Line":7,"Col":6}}
		]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if got := reports[0].Funcs[0]; got.Name != "Unused" || got.Position.Line != 7 {
		t.Fatalf("decoded function = %#v", got)
	}
}

func TestAnalyzerArgsUseProductionRoots(t *testing.T) {
	t.Parallel()
	got := analyzerArgs([]string{"./cmd/...", "./internal/..."})
	want := []string{"-json", "./cmd/...", "./internal/..."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("analyzer args = %q, want %q", got, want)
	}
}

func TestExemptionProblemsIgnoreTestSupportPackages(t *testing.T) {
	t.Parallel()
	reports := allPlatformsReport([]reportPackage{{
		Path: "github.com/goobers/goobers/test/testsupport/fake",
		Funcs: []reportFunction{{
			Name:     "New",
			Position: reportPosition{File: "test/testsupport/fake/fake.go", Line: 10},
		}},
	}})
	if problems := exemptionProblems(reports, nil); len(problems) != 0 {
		t.Fatalf("test support problems = %v", problems)
	}
}

// allPlatformsReport fixtures the common case: the same report package list
// reported dead identically on every target platform (a genuinely
// platform-neutral symbol).
func allPlatformsReport(packages []reportPackage) map[string][]reportPackage {
	reports := make(map[string][]reportPackage, len(targetGOOS))
	for _, goos := range targetGOOS {
		reports[goos] = packages
	}
	return reports
}
