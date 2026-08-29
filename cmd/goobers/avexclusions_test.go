package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/avexclusion"
	"github.com/goobers/goobers/internal/dispatcher"
)

// avExclusionInstanceYAML declares one windows runner with the claim and one
// without, so the doctor report's declaration column has both states to show.
const avExclusionInstanceYAML = `apiVersion: goobers.dev/v1alpha1
kind: Instance
schemaVersion: 2
repos: []
runners:
  - name: self
    host: self
    provides:
      os: linux
  - name: windows-shell
    host: ghcr.io/example/goobers-windows:v1
    provides:
      os: windows
      shell: true
  - name: windows-verified
    host: ghcr.io/example/goobers-windows:v1
    provides:
      os: windows
      shell: true
      windows:
        avExclusionsVerified: true
engine:
  hostPort: temporal.example:7233
`

func writeAVExclusionInstance(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte(avExclusionInstanceYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestDoctorAVExclusionsWindowsReport is the verification half on a Windows
// host: the exclusion list is read once, every enumerated directory is
// judged against it, the report names which entry covers what and which
// directories are uncovered, the declaration column shows each windows
// runner's claim state, and the exit code stays 0 with directories
// uncovered — advisory by the issue's own scope note.
func TestDoctorAVExclusionsWindowsReport(t *testing.T) {
	root := writeAVExclusionInstance(t)
	workRoot := filepath.Join(root, "worker")
	calls := 0
	deps := avExclusionDeps{
		hostOS:  "windows",
		tempDir: filepath.Join(root, "tmp"),
		query: func(context.Context) ([]string, error) {
			calls++
			return []string{root, `C:\Users\*\AppData\Local\Temp`}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	if code := runDoctorAVExclusions(root, workRoot, "text", &stdout, &stderr, deps); code != 0 {
		t.Fatalf("code = %d, want 0 (advisory); stderr=%s", code, stderr.String())
	}
	if calls != 1 {
		t.Fatalf("exclusion list read %d times, want once", calls)
	}
	text := stdout.String()
	for _, want := range []string{
		"AV EXCLUSIONS (advisory, #3480)",
		"Microsoft Defender exclusions read: 2 entries",
		"EXCLUDED (by " + root + ") daemon       " + root,
		"EXCLUDED (by " + root + ") worker       " + workRoot, // under the instance root here
		`EXCLUDED (by C:\Users\*\AppData\Local\Temp) stage-pod    ` + dispatcher.WindowsTmpPath,
		"NOT-EXCLUDED  stage-pod    " + dispatcher.WindowsWorkspacePath,
		"NOT-EXCLUDED  stage-pod    " + dispatcher.WindowsHomePath,
		"windows-shell: undeclared (RNR006)",
		"windows-verified: true",
		"Nothing here changes host antivirus settings",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text report missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "self:") {
		t.Errorf("a linux runner must not appear in the declaration column:\n%s", text)
	}

	stdout.Reset()
	if code := runDoctorAVExclusions(root, workRoot, "json", &stdout, &stderr, deps); code != 0 {
		t.Fatalf("json code = %d, want 0; stderr=%s", code, stderr.String())
	}
	var report doctorAVExclusionsReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode json report: %v\n%s", err, stdout.String())
	}
	if report.Host != "windows" || !report.Queried || report.QueryError != "" || len(report.Exclusions) != 2 {
		t.Errorf("report header = %+v", report)
	}
	if report.InstanceRoot != root || report.WorkRoot != workRoot {
		t.Errorf("roots = %q/%q, want %q/%q", report.InstanceRoot, report.WorkRoot, root, workRoot)
	}
	byPath := map[string]avexclusion.Finding{}
	for _, f := range report.Findings {
		byPath[f.Path] = f
	}
	if f := byPath[dispatcher.WindowsWorkspacePath]; f.Coverage != avexclusion.CoverageNotExcluded || f.Role != avexclusion.RoleStagePod {
		t.Errorf("workspace finding = %+v", f)
	}
	if f := byPath[filepath.Join(root, "workcopies")]; f.Coverage != avexclusion.CoverageExcluded || f.MatchedBy != root {
		t.Errorf("workcopies finding = %+v", f)
	}
	if len(report.Runners) != 2 || report.Runners[0].Name != "windows-shell" || report.Runners[0].AVExclusionsVerified != nil ||
		report.Runners[1].Name != "windows-verified" || report.Runners[1].AVExclusionsVerified == nil || !*report.Runners[1].AVExclusionsVerified {
		t.Errorf("runners = %+v", report.Runners)
	}
}

// TestDoctorAVExclusionsOffWindowsListsWithoutQuerying is the declaration
// half on the operator's laptop: the list is still produced (that is what
// an operator feeds to their tooling), nothing is queried, every entry is
// unknown with the reason, exit 0.
func TestDoctorAVExclusionsOffWindowsListsWithoutQuerying(t *testing.T) {
	root := writeAVExclusionInstance(t)
	deps := avExclusionDeps{
		hostOS:  "darwin",
		tempDir: filepath.Join(root, "tmp"),
		query: func(context.Context) ([]string, error) {
			t.Fatal("exclusion list must not be read off Windows")
			return nil, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := runDoctorAVExclusions(root, "", "text", &stdout, &stderr, deps); code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"verification: unknown — verification runs on Windows only (this host is darwin)",
		"UNKNOWN       daemon       " + root,
		"UNKNOWN       worker       " + defaultWorkerRoot(deps.tempDir),
		"UNKNOWN       stage-pod    " + dispatcher.WindowsWorkspacePath,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text report missing %q:\n%s", want, text)
		}
	}
}

// TestDoctorAVExclusionsQueryFailureIsReportedNotFatal: a host whose
// Defender cannot be read (no PowerShell, no cmdlet, timeout) still gets
// the list, with the failure quoted — never a silent pass, never exit 1.
func TestDoctorAVExclusionsQueryFailureIsReportedNotFatal(t *testing.T) {
	root := t.TempDir() // no instance.yaml: the layout defaults stand
	deps := avExclusionDeps{
		hostOS:  "windows",
		tempDir: filepath.Join(root, "tmp"),
		query: func(context.Context) ([]string, error) {
			return nil, errors.New("Get-MpPreference: exit status 1: The term 'Get-MpPreference' is not recognized")
		},
	}
	var stdout, stderr bytes.Buffer
	if code := runDoctorAVExclusions(root, "", "json", &stdout, &stderr, deps); code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "instance.yaml not found; listing the default layout") {
		t.Errorf("missing-instance note absent from stderr: %q", stderr.String())
	}
	var report doctorAVExclusionsReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Queried || !strings.Contains(report.QueryError, "Get-MpPreference") || len(report.Findings) == 0 {
		t.Errorf("report = %+v", report)
	}
	for _, f := range report.Findings {
		if f.Coverage != avexclusion.CoverageUnknown {
			t.Errorf("%s coverage = %q, want unknown", f.Path, f.Coverage)
		}
	}
}

// TestDoctorAVExclusionsUsage pins the CLI surface: two positional roots
// are a usage error, and the mode is reachable through the real entry.
func TestDoctorAVExclusionsUsage(t *testing.T) {
	if code, _, _ := runArgs(t, "doctor", "--av-exclusions", "a", "b"); code != 2 {
		t.Errorf("two positional roots: code = %d, want 2", code)
	}
	code, stdout, _ := runArgs(t, "doctor", "--av-exclusions", "--report", "json", t.TempDir())
	if code != 0 || !strings.Contains(stdout, `"findings"`) {
		t.Errorf("code = %d, stdout = %q; want a JSON report and exit 0 on any host", code, stdout)
	}
}

// TestStagePodAVExclusionAdvisory is the pod-entrypoint line: derived from
// the pod's real environment (working directory, TMP, USERPROFILE,
// GOOBERS_INSTANCE_ROOT), one line, naming the uncovered directories —
// and nothing at all off Windows.
func TestStagePodAVExclusionAdvisory(t *testing.T) {
	env := map[string]string{
		"TMP":                   dispatcher.WindowsTmpPath,
		"USERPROFILE":           dispatcher.WindowsHomePath,
		"GOOBERS_INSTANCE_ROOT": `C:\instance`,
	}
	getenv := func(k string) string { return env[k] }
	getwd := func() (string, error) { return dispatcher.WindowsWorkspacePath, nil }

	deps := avExclusionDeps{hostOS: "windows", query: func(context.Context) ([]string, error) {
		return []string{`C:\workspace`, `C:\instance`}, nil
	}}
	line := stagePodAVExclusionAdvisory(context.Background(), deps, getwd, getenv)
	if strings.Contains(line, "\n") {
		t.Fatalf("advisory spans lines: %q", line)
	}
	for _, want := range []string{
		"av-exclusions (advisory, stage pod)",
		"2 of 4 directories",
		"NOT excluded: " + dispatcher.WindowsTmpPath + " (stage temp directory",
		dispatcher.WindowsHomePath + " (container user profile",
		"#3480",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("advisory %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "NOT excluded: "+dispatcher.WindowsWorkspacePath) {
		t.Errorf("advisory reports the covered workspace as missing: %q", line)
	}

	// A working directory the pod cannot resolve falls back to the
	// dispatcher's mount contract rather than dropping the workspace.
	failing := func() (string, error) { return "", errors.New("getwd: access denied") }
	if line := stagePodAVExclusionAdvisory(context.Background(), deps, failing, getenv); !strings.Contains(line, "4 directories") {
		t.Errorf("getwd failure dropped the workspace: %q", line)
	}

	// Off Windows: silent, and the reader is never invoked.
	linux := avExclusionDeps{hostOS: "linux", query: func(context.Context) ([]string, error) {
		t.Fatal("exclusion list must not be read off Windows")
		return nil, nil
	}}
	if line := stagePodAVExclusionAdvisory(context.Background(), linux, getwd, getenv); line != "" {
		t.Errorf("linux advisory = %q, want empty", line)
	}
	if line := hostAVExclusionAdvisory(context.Background(), "daemon", nil, linux); line != "" {
		t.Errorf("linux host advisory = %q, want empty", line)
	}
}

// TestWorkerAVExclusionDirectoriesFollowTheProvisioner: the worker's set is
// resolved through the same helpers workerEngineDepsForPlatform provisions
// with, so a renamed subtree cannot leave the advisory naming a stale path.
func TestWorkerAVExclusionDirectoriesFollowTheProvisioner(t *testing.T) {
	workRoot := t.TempDir()
	dirs := workerAVExclusionDirectories(workRoot, avExclusionDeps{tempDir: filepath.Join(workRoot, "tmp")})
	paths := map[string]bool{}
	for _, d := range dirs {
		paths[d.Path] = true
	}
	for _, want := range []string{workRoot, workerWorkcopiesDir(workRoot), workerScratchDir(workRoot), filepath.Join(workRoot, "tmp")} {
		if !paths[want] {
			t.Errorf("worker set %v lacks %s", dirs, want)
		}
	}
}
