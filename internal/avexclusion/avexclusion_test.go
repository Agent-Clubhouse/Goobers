package avexclusion

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// TestCovers pins the matching rules an operator's exclusion list is read
// under: Defender compares case-insensitively, an excluded folder covers its
// subtree, entries may use either slash and a trailing separator, and the
// two Defender wildcards match. The sibling-prefix case is the one that
// bites: `C:\inst` must not be read as covering `C:\instance`.
func TestCovers(t *testing.T) {
	cases := []struct {
		name       string
		exclusions []string
		path       string
		want       bool
		wantBy     string
	}{
		{name: "exact", exclusions: []string{`C:\instance`}, path: `C:\instance`, want: true, wantBy: `C:\instance`},
		{name: "ancestor", exclusions: []string{`C:\instance`}, path: `C:\instance\workcopies\e2e`, want: true, wantBy: `C:\instance`},
		{name: "case-insensitive", exclusions: []string{`c:\INSTANCE`}, path: `C:\instance\runs`, want: true, wantBy: `c:\INSTANCE`},
		{name: "forward slashes and trailing separator", exclusions: []string{`C:/instance/`}, path: `C:\instance\runs`, want: true},
		{name: "sibling prefix is not an ancestor", exclusions: []string{`C:\inst`}, path: `C:\instance`, want: false},
		{name: "unrelated", exclusions: []string{`D:\elsewhere`, `C:\Users\alice`}, path: `C:\instance`, want: false},
		{name: "wildcard star", exclusions: []string{`C:\Users\*\AppData\Local\Temp`}, path: `C:\Users\ContainerUser\AppData\Local\Temp`, want: true},
		{name: "wildcard covers subtree", exclusions: []string{`C:\Users\*\AppData\Local\Temp`}, path: `C:\Users\ContainerUser\AppData\Local\Temp\goobers-worker`, want: true},
		{name: "wildcard question mark", exclusions: []string{`?:\workspace`}, path: `C:\workspace`, want: true},
		{name: "wildcard no match", exclusions: []string{`C:\Users\*\Documents`}, path: `C:\Users\ContainerUser\AppData\Local\Temp`, want: false},
		// The dangerous direction: a `*` that spanned separators would call
		// a profile nested two levels deeper EXCLUDED on the strength of a
		// one-level rule — an affirmative all-clear over a directory the
		// host is still scanning.
		{name: "star does not span a separator", exclusions: []string{`C:\Users\*\AppData\Local\Temp`}, path: `C:\Users\a\b\c\AppData\Local\Temp`, want: false},
		{name: "one star per level, as Defender writes it", exclusions: []string{`C:\Users\*\*\AppData`}, path: `C:\Users\a\b\AppData`, want: true},
		{name: "question mark does not match a separator", exclusions: []string{`C:\a?b`}, path: `C:\a\b`, want: false},
		{name: "trailing star still covers one level", exclusions: []string{`C:\workspace\*`}, path: `C:\workspace\repo`, want: true},
		// The safe direction, documented on Covers: a spurious warning, not
		// a false all-clear.
		{name: "trailing star does not cover the directory itself", exclusions: []string{`C:\workspace\*`}, path: `C:\workspace`, want: false},
		{name: "whole drive", exclusions: []string{`C:\`}, path: `C:\workspace`, want: true},
		{name: "empty path", exclusions: []string{`C:\`}, path: "", want: false},
		{name: "blank entry ignored", exclusions: []string{"", "  "}, path: `C:\workspace`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			by, got := Covers(tc.exclusions, tc.path)
			if got != tc.want {
				t.Fatalf("Covers(%q, %q) = %v, want %v", tc.exclusions, tc.path, got, tc.want)
			}
			if tc.wantBy != "" && by != tc.wantBy {
				t.Fatalf("matched by %q, want %q", by, tc.wantBy)
			}
		})
	}
}

func TestVerifyMarksEveryDirectoryUnknownWhenNotQueried(t *testing.T) {
	dirs := StagePodDirectories(`C:\workspace`, `C:\Users\ContainerUser\AppData\Local\Temp`, "", "")
	report := Verify(dirs, nil, false, errors.New("powershell.exe: executable file not found"))
	if report.Queried || !strings.Contains(report.QueryError, "powershell.exe") {
		t.Fatalf("report = %+v, want unqueried with the probe error", report)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("findings = %d, want the 2 non-empty directories", len(report.Findings))
	}
	for _, f := range report.Findings {
		if f.Coverage != CoverageUnknown {
			t.Fatalf("%s coverage = %q, want %q", f.Path, f.Coverage, CoverageUnknown)
		}
	}
	if report.Exclusions != nil {
		t.Fatalf("exclusions = %v, want none recorded for an unqueried report", report.Exclusions)
	}
}

func TestVerifySplitsExcludedFromNotExcluded(t *testing.T) {
	dirs := StagePodDirectories(`C:\workspace`, `C:\Users\ContainerUser\AppData\Local\Temp`, `C:\Users\ContainerUser`, `C:\instance`)
	report := Verify(dirs, []string{`C:\workspace`, `%USERPROFILE%`, `C:\Users\ContainerUser\AppData\Local\Temp`}, true, nil)
	if !report.Queried || report.QueryError != "" {
		t.Fatalf("report = %+v, want queried", report)
	}
	want := map[string]Coverage{
		`C:\workspace`: CoverageExcluded,
		`C:\Users\ContainerUser\AppData\Local\Temp`: CoverageExcluded,
		`C:\Users\ContainerUser`:                    CoverageNotExcluded, // %USERPROFILE% unexpanded: the probe expands, Go does not
		`C:\instance`:                               CoverageNotExcluded,
	}
	for _, f := range report.Findings {
		if f.Coverage != want[f.Path] {
			t.Errorf("%s coverage = %q, want %q", f.Path, f.Coverage, want[f.Path])
		}
		if f.Coverage == CoverageExcluded && f.MatchedBy == "" {
			t.Errorf("%s excluded without naming the matching entry", f.Path)
		}
	}
}

// TestDaemonDirectoriesDeriveFromLayout is the no-drift property: the set
// is read off instance.Layout, so a relocated workcopies root shows up as
// its own entry, and every path is one the Layout itself resolves.
func TestDaemonDirectoriesDeriveFromLayout(t *testing.T) {
	root := filepath.Join("C:", "instance")
	layout := instance.NewLayout(root).WithWorkcopiesRoot(filepath.Join("D:", "wc"))
	dirs := DaemonDirectories(layout, filepath.Join("C:", "Temp"))
	paths := make(map[string]Directory, len(dirs))
	for _, d := range dirs {
		if d.Role != RoleDaemon {
			t.Errorf("%s role = %q, want %q", d.Path, d.Role, RoleDaemon)
		}
		if d.Purpose == "" {
			t.Errorf("%s has no purpose", d.Path)
		}
		paths[d.Path] = d
	}
	for _, want := range []string{root, layout.RunsDir(), layout.GagglesDir(), layout.SchedulerDir(), layout.BlobStoreDir(), layout.WorkcopiesBaseDir(), filepath.Join("C:", "Temp")} {
		if _, ok := paths[want]; !ok {
			t.Errorf("missing %s in %v", want, dirs)
		}
	}
	if _, ok := paths[filepath.Join("D:", "wc")]; !ok {
		t.Errorf("relocated workcopies root not listed: %v", dirs)
	}
	if got := DaemonDirectories(layout, ""); len(got) != len(dirs)-1 {
		t.Errorf("empty temp dir should be omitted: %d entries, want %d", len(got), len(dirs)-1)
	}
}

// TestGaggleWorkcopiesDirectoryFollowsTheGaggleOverride is the hole the
// instance-wide-only enumeration left: a gaggle's own workcopies.root beats
// the instance-wide one, so the directory that actually holds that gaggle's
// mirrors and worktrees is not under the instance root at all. It must be
// enumerated on its own, and Dedupe must still collapse the ordinary case
// where the gaggle inherits.
func TestGaggleWorkcopiesDirectoryFollowsTheGaggleOverride(t *testing.T) {
	root := filepath.Join("C:", "instance")
	base := instance.NewLayout(root)

	// Gaggle override wins over the instance-wide root, and keeps its own
	// gaggle segment so two gaggles cannot share mutable worktrees.
	relocated := base.ForGaggle("builders").WithWorkcopiesRoot(filepath.Join("D:", "fast-ssd"))
	dir := GaggleWorkcopiesDirectory("builders", relocated)
	if want := filepath.Join("D:", "fast-ssd", "builders"); dir.Path != want {
		t.Errorf("relocated gaggle path = %q, want %q", dir.Path, want)
	}
	if dir.Role != RoleDaemon || !strings.Contains(dir.Purpose, "builders") {
		t.Errorf("entry = %+v, want a daemon entry naming the gaggle", dir)
	}

	// A relocated gaggle is NOT covered by the instance-root entry: the
	// whole point is that it appears as its own finding.
	instanceSet := DaemonDirectories(base, "")
	report := Verify(append(instanceSet, dir), []string{root}, true, nil)
	var found bool
	for _, f := range report.Findings {
		if f.Path != dir.Path {
			continue
		}
		found = true
		if f.Coverage != CoverageNotExcluded {
			t.Errorf("relocated gaggle coverage = %q, want %q", f.Coverage, CoverageNotExcluded)
		}
	}
	if !found {
		t.Fatalf("relocated gaggle root missing from %+v", report.Findings)
	}
	if line := Summary("daemon", report); !strings.Contains(line, dir.Path) || strings.Contains(line, "every enumerated directory is covered") {
		t.Errorf("summary must name the uncovered gaggle root, got %q", line)
	}

	// The inheriting case: the gaggle's base dir is the instance-wide one,
	// so Dedupe collapses it rather than printing the same path twice.
	inherited := GaggleWorkcopiesDirectory("e2e", base.ForGaggle("e2e"))
	if got := Dedupe(append(DaemonDirectories(base, ""), inherited)); len(got) != len(instanceSet) {
		t.Errorf("Dedupe = %d entries, want the inherited gaggle root collapsed into %d", len(got), len(instanceSet))
	}
}

func TestWorkerDirectoriesAndDedupe(t *testing.T) {
	dirs := WorkerDirectories(`C:\Temp\goobers-worker`, `C:\Temp\goobers-worker\workcopies`, `C:\Temp\goobers-worker\scratch`, `C:\Temp`)
	if len(dirs) != 4 {
		t.Fatalf("dirs = %v, want 4", dirs)
	}
	dup := append(append([]Directory(nil), dirs...), Directory{Role: RoleDaemon, Path: `c:/temp/`})
	if got := Dedupe(dup); len(got) != 4 {
		t.Fatalf("Dedupe = %v, want the case/slash-variant temp entry dropped", got)
	}
}

func TestSummaryIsOneLineNamingTheGap(t *testing.T) {
	dirs := StagePodDirectories(`C:\workspace`, `C:\Users\ContainerUser\AppData\Local\Temp`, "", "")
	line := Summary("stage pod", Verify(dirs, []string{`C:\workspace`}, true, nil))
	if strings.Contains(line, "\n") {
		t.Fatalf("summary spans lines: %q", line)
	}
	for _, want := range []string{"av-exclusions (advisory, stage pod)", "1 of 2", `NOT excluded: C:\Users\ContainerUser\AppData\Local\Temp (stage temp directory`, "#3480"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, `NOT excluded: C:\workspace`) {
		t.Errorf("summary names the covered directory as missing: %q", line)
	}

	covered := Summary("daemon", Verify(dirs, []string{`C:\`}, true, nil))
	if !strings.Contains(covered, "2 of 2") || !strings.Contains(covered, "every enumerated directory is covered") {
		t.Errorf("all-covered summary = %q", covered)
	}

	unknown := Summary("worker", Verify(dirs, nil, false, errors.New("Get-MpPreference: exit status 1")))
	for _, want := range []string{"could not read Microsoft Defender exclusions (Get-MpPreference: exit status 1)", `C:\workspace (stage workspace mount`, `C:\Users\ContainerUser\AppData\Local\Temp (`} {
		if !strings.Contains(unknown, want) {
			t.Errorf("unknown summary %q lacks %q", unknown, want)
		}
	}
}

// TestStagePodQueryIsBoundedTighterThanTheDaemons: a stage pod pays the
// Defender probe on EVERY attempt, and on the Server Core images this
// feature anticipates it pays it for an answer that is always unknown — so
// its bound is shorter than the daemon's, and the error a caller sees names
// the bound that caller actually chose rather than a constant.
func TestStagePodQueryIsBoundedTighterThanTheDaemons(t *testing.T) {
	if StagePodQueryTimeout >= DefenderQueryTimeout {
		t.Errorf("StagePodQueryTimeout = %s, want less than the daemon's %s", StagePodQueryTimeout, DefenderQueryTimeout)
	}
	// A bound this small expires before the probe can produce anything, on
	// any host — the point is which duration the message names.
	if _, err := QueryDefenderWithin(context.Background(), time.Nanosecond); err == nil ||
		!strings.Contains(err.Error(), "timed out after 1ns") {
		t.Errorf("QueryDefenderWithin error = %v, want one naming the 1ns bound it was given", err)
	}
}

func TestParseExclusionList(t *testing.T) {
	got := ParseExclusionList([]byte("C:\\instance\r\n\r\n  D:\\wc  \nC:\\Users\\alice\\AppData\\Local\\Temp\n"))
	want := []string{`C:\instance`, `D:\wc`, `C:\Users\alice\AppData\Local\Temp`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ParseExclusionList = %q, want %q", got, want)
	}
	if got := ParseExclusionList(nil); got != nil {
		t.Fatalf("empty output = %q, want nil", got)
	}
}
