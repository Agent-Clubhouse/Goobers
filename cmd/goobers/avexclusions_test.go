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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/avexclusion"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workerhost"
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

// writeAVExclusionGaggle adds a config/ directory declaring one gaggle whose
// spec.workcopies.root points OUTSIDE the instance root — the override that
// beats the instance-wide one and that the daemon resolves per gaggle.
func writeAVExclusionGaggle(t *testing.T, root, gaggle, workcopiesRoot string) {
	t.Helper()
	dir := filepath.Join(root, "config", "gaggles", gaggle)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: av-exclusions
spec:
  instance:
    name: av-exclusions
    environment: dev
  connections:
    - name: repo-token
      type: repo
      provider: github
      secretRef:
        name: repo-token
  gaggles:
    - ` + gaggle + "\n"
	if err := os.WriteFile(filepath.Join(root, "config", "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	def := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: ` + gaggle + `
spec:
  displayName: Builders
  project:
    provider: github
    owner: example
    name: repo
    branch: main
    connectionRef: repo-token
  backlog:
    provider: github
    project: example/repo
    labels:
      - goobers
    connectionRef: repo-token
  workcopies:
    root: ` + workcopiesRoot + `
  isolation:
    namespace: gaggle-` + gaggle + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gaggle.yaml"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorAVExclusionsEnumeratesGaggleWorkcopiesRoots is the hole a
// gaggle-level workcopies.root left open. That override wins over the
// instance-wide one, the daemon resolves it per gaggle, and the directory it
// names holds that gaggle's git mirrors and per-run worktrees — the exact
// write-then-read hot spot #3161–#3164 describes. Enumerated only from
// instance.yaml, the doctor listed it nowhere and the advisory reported an
// affirmative all-clear over a directory it had never looked at.
func TestDoctorAVExclusionsEnumeratesGaggleWorkcopiesRoots(t *testing.T) {
	root := writeAVExclusionInstance(t)
	fastSSD := filepath.Join(t.TempDir(), "fast-ssd")
	writeAVExclusionGaggle(t, root, "builders", fastSSD)
	relocated := filepath.Join(fastSSD, "builders")

	deps := avExclusionDeps{
		hostOS:  "windows",
		tempDir: filepath.Join(root, "tmp"),
		// An operator who excluded everything the previous list named.
		query: func(context.Context) ([]string, error) {
			return []string{root, filepath.Join(root, "tmp")}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	if code := runDoctorAVExclusions(root, filepath.Join(root, "worker"), "json", &stdout, &stderr, deps); code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	var report doctorAVExclusionsReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode json report: %v\n%s", err, stdout.String())
	}
	var finding *avexclusion.Finding
	for i := range report.Findings {
		if report.Findings[i].Path == relocated {
			finding = &report.Findings[i]
		}
	}
	if finding == nil {
		t.Fatalf("gaggle workcopies root %s missing from the report:\n%s", relocated, stdout.String())
	}
	if finding.Coverage != avexclusion.CoverageNotExcluded {
		t.Errorf("relocated gaggle root coverage = %q, want %q", finding.Coverage, avexclusion.CoverageNotExcluded)
	}
	if !strings.Contains(finding.Purpose, "builders") {
		t.Errorf("finding %+v does not name the gaggle it belongs to", *finding)
	}
	// The advisory the daemon prints must not call this covered.
	if line := avexclusion.Summary("daemon", report.Report); !strings.Contains(line, relocated) ||
		strings.Contains(line, "every enumerated directory is covered") {
		t.Errorf("summary must name the uncovered gaggle root, got %q", line)
	}

	// The text report names it too, so an operator feeding their tooling
	// from `doctor` gets the path.
	stdout.Reset()
	if code := runDoctorAVExclusions(root, filepath.Join(root, "worker"), "text", &stdout, &stderr, deps); code != 0 {
		t.Fatalf("text code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "NOT-EXCLUDED  daemon       "+relocated) {
		t.Errorf("text report missing the relocated gaggle root:\n%s", stdout.String())
	}
}

// TestDaemonAVExclusionDirectoriesResolveBothWorkcopiesOverrides pins the
// derivation `goobers up`'s startup advisory and `goobers doctor` share: a
// gaggle's own workcopies.root BEATS the instance-wide one (the daemon
// resolves it that way per gaggle when it builds each worktree.Manager), so
// both roots must appear, each as its own entry.
func TestDaemonAVExclusionDirectoriesResolveBothWorkcopiesOverrides(t *testing.T) {
	root := t.TempDir()
	instanceWide := filepath.Join(t.TempDir(), "shared-wc")
	gaggleRoot := filepath.Join(t.TempDir(), "fast-ssd")

	layout := instance.NewLayout(root)
	cfg := &instance.Config{Workcopies: &instance.WorkcopiesConfig{Root: instanceWide}}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "builders"},
			Spec:       apiv1.GaggleSpec{Workcopies: &apiv1.GaggleWorkcopies{Root: gaggleRoot}},
		},
		{ObjectMeta: metav1.ObjectMeta{Name: "inherits"}},
	}}

	dirs := avexclusion.Dedupe(daemonAVExclusionDirectories(layout, cfg, set, avExclusionDeps{tempDir: filepath.Join(root, "tmp")}))
	paths := map[string]string{}
	for _, d := range dirs {
		paths[d.Path] = d.Purpose
	}
	// The gaggle keeps its own segment under the override, so two gaggles
	// cannot share mutable worktrees.
	if _, ok := paths[filepath.Join(gaggleRoot, "builders")]; !ok {
		t.Errorf("gaggle-level workcopies.root missing from %v", dirs)
	}
	// The instance-wide override still stands for the gaggle that inherits.
	if _, ok := paths[filepath.Join(instanceWide, "inherits")]; !ok {
		t.Errorf("instance-wide workcopies.root missing for the inheriting gaggle: %v", dirs)
	}
	// With no config set the per-gaggle entries are simply absent — the
	// caller is responsible for saying so, and both do.
	without := avexclusion.Dedupe(daemonAVExclusionDirectories(layout, cfg, nil, avExclusionDeps{tempDir: filepath.Join(root, "tmp")}))
	if len(without) >= len(dirs) {
		t.Errorf("nil config set produced %d entries, want fewer than %d", len(without), len(dirs))
	}
}

// TestDoctorAVExclusionsSaysWhenGagglesCouldNotBeEnumerated: a config
// directory that will not load must never pass silently for "no gaggles" —
// that is the same false all-clear, one level up.
func TestDoctorAVExclusionsSaysWhenGagglesCouldNotBeEnumerated(t *testing.T) {
	root := writeAVExclusionInstance(t)
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "manifest.yaml"), []byte(":\n\tnot yaml at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := avExclusionDeps{hostOS: "darwin", tempDir: filepath.Join(root, "tmp")}
	var stdout, stderr bytes.Buffer
	if code := runDoctorAVExclusions(root, filepath.Join(root, "worker"), "text", &stdout, &stderr, deps); code != 0 {
		t.Fatalf("code = %d, want 0 (advisory); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "per-gaggle workcopies roots are NOT enumerated") {
		t.Errorf("an unloadable config directory must be reported, got stderr=%q", stderr.String())
	}
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
	// --work-root scopes --av-exclusions alone. Parsed and ignored in the
	// other modes it would let an operator believe they had scoped a check
	// they had not.
	for _, args := range [][]string{
		{"doctor", "--k8s", "--work-root", t.TempDir()},
		{"doctor", "--repo", "--work-root", t.TempDir()},
	} {
		code, _, stderr := runArgs(t, args...)
		if code != 2 || !strings.Contains(stderr, "--work-root applies to --av-exclusions only") {
			t.Errorf("%v: code = %d, stderr = %q; want a usage error", args, code, stderr)
		}
	}
	if code, _, stderr := runArgs(t, "doctor", "--av-exclusions", "--work-root", t.TempDir(), t.TempDir()); code != 0 {
		t.Errorf("--work-root with --av-exclusions: code = %d, want 0; stderr=%q", code, stderr)
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

// TestWorkerAVExclusionDirectoriesFollowTheProvisioner: the advisory's
// worker set is the set workerEngineDepsForPlatform ACTUALLY provisions.
//
// The assertion is deliberately made against the runtime the provisioner
// returns — the worktree manager's own root and the workspace
// provisioner's own scratch directory — rather than against
// workerWorkcopiesDir/workerScratchDir, which are the helpers the function
// under test calls. Asserting against those would be tautological: it
// cannot fail, and it would stay green if the provisioner were re-pointed
// at a literal path, which is exactly the drift the advisory must not have.
func TestWorkerAVExclusionDirectoriesFollowTheProvisioner(t *testing.T) {
	workRoot := t.TempDir()
	runtimeDeps, err := workerEngineDepsForPlatform(workRoot, "linux", "test-owner")
	if err != nil {
		t.Fatalf("provision worker engine deps: %v", err)
	}
	t.Cleanup(func() { _ = runtimeDeps.Close() })

	provisioned, ok := runtimeDeps.deps.Workspaces.(*workerhost.WorktreeWorkspaces)
	if !ok {
		t.Fatalf("worker workspaces = %T, want *workerhost.WorktreeWorkspaces", runtimeDeps.deps.Workspaces)
	}
	tempDir := filepath.Join(workRoot, "tmp")
	dirs := workerAVExclusionDirectories(workRoot, avExclusionDeps{tempDir: tempDir})
	paths := map[string]bool{}
	for _, d := range dirs {
		paths[d.Path] = true
	}
	// worktree.NewManager absolutises its root, so compare like for like.
	wantWorkcopies, err := filepath.Abs(workerWorkcopiesDir(workRoot))
	if err != nil {
		t.Fatal(err)
	}
	if provisioned.Manager.Root != wantWorkcopies {
		t.Fatalf("provisioner mirror root = %q, want %q", provisioned.Manager.Root, wantWorkcopies)
	}
	for _, want := range []string{workRoot, provisioned.Manager.Root, provisioned.ScratchDir, tempDir} {
		if !paths[want] {
			t.Errorf("worker advisory set %v does not name the provisioned directory %s", dirs, want)
		}
	}
}
