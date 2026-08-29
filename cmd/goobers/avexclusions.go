package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/goobers/goobers/internal/avexclusion"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
)

// #3480: the antivirus-exclusion declaration and advisory check, wired into
// the three processes that write-then-read on a Windows host — the daemon
// (`goobers up`), the worker (`goobers worker`) and the dispatcher-created
// stage pod (`goobers __dispatch-exec`) — each printing ONE advisory line at
// startup, plus `goobers doctor --av-exclusions`, which enumerates the whole
// set for an operator's own tooling. The directory set comes from
// internal/avexclusion, which derives it from the same path code the
// processes use; nothing here retypes a path.

// avExclusionDeps is the seam the advisory runs through: the host OS
// (verification is Windows-only; elsewhere the set is listed and coverage
// reported unknown), the exclusion-list reader, and the process temp
// directory.
type avExclusionDeps struct {
	hostOS  string
	query   avexclusion.Querier
	tempDir string
}

func realAVExclusionDeps() avExclusionDeps {
	return avExclusionDeps{hostOS: runtime.GOOS, query: avexclusion.QueryDefender, tempDir: filepath.Clean(os.TempDir())}
}

// realStagePodAVExclusionDeps is the pod's variant: the same reader under
// avexclusion.StagePodQueryTimeout, because a stage pod pays the probe on
// every attempt (see that constant's doc comment).
func realStagePodAVExclusionDeps() avExclusionDeps {
	deps := realAVExclusionDeps()
	deps.query = avexclusion.QueryDefenderStagePod
	return deps
}

// avExclusionReport verifies dirs against the host's exclusion list. Off
// Windows there is nothing to read: every directory is reported unknown with
// the reason, so a `doctor --av-exclusions` run on an operator's macOS
// laptop still yields the list.
func avExclusionReport(ctx context.Context, dirs []avexclusion.Directory, deps avExclusionDeps) avexclusion.Report {
	dirs = avexclusion.Dedupe(dirs)
	if deps.hostOS != "windows" {
		return avexclusion.Verify(dirs, nil, false, fmt.Errorf("verification runs on Windows only (this host is %s)", deps.hostOS))
	}
	if deps.query == nil {
		return avexclusion.Verify(dirs, nil, false, fmt.Errorf("no exclusion-list reader configured"))
	}
	exclusions, err := deps.query(ctx)
	if err != nil {
		return avexclusion.Verify(dirs, nil, false, err)
	}
	return avexclusion.Verify(dirs, exclusions, true, nil)
}

// hostAVExclusionAdvisory is the one-line startup advisory the daemon and
// the worker print. Empty off Windows: a Linux daemon has no AV race to
// warn about, and the existing large-repo preflight keeps the same silence.
func hostAVExclusionAdvisory(ctx context.Context, label string, dirs []avexclusion.Directory, deps avExclusionDeps) string {
	if deps.hostOS != "windows" {
		return ""
	}
	return avexclusion.Summary(label, avExclusionReport(ctx, dirs, deps))
}

// daemonAVExclusionDirectories is the daemon's set for the instance rooted
// at layout, honouring BOTH workcopies.root overrides the daemon itself
// applies (instance.EffectiveWorkcopiesLayout): the instance-wide one, and
// each gaggle's own — which wins over it, points at any absolute path, and
// is where that gaggle's git mirrors and per-run worktrees actually live.
// Enumerating only the instance-wide root would let a gaggle relocated to
// another drive go unnamed and unjudged while Summary printed an
// affirmative all-clear (see avexclusion.GaggleWorkcopiesDirectory).
//
// cfg may be nil (no instance.yaml yet): the layout defaults stand. set may
// be nil (no config directory, or one that would not load): the per-gaggle
// entries are simply absent, and every caller that can say so says so.
func daemonAVExclusionDirectories(layout instance.Layout, cfg *instance.Config, set *instance.ConfigSet, deps avExclusionDeps) []avexclusion.Directory {
	instanceWide := layout
	if effective, err := instance.EffectiveWorkcopiesLayout(layout, cfg, nil); err == nil {
		instanceWide = effective
	}
	dirs := avexclusion.DaemonDirectories(instanceWide, deps.tempDir)
	if set == nil {
		return dirs
	}
	// The same resolution, per gaggle, that daemon.go performs when it
	// builds each gaggle's worktree.Manager — layout.ForGaggle first, so
	// the gaggle segment is present, then the override.
	for i := range set.Gaggles {
		gaggle := &set.Gaggles[i]
		scoped, err := instance.EffectiveWorkcopiesLayout(layout.ForGaggle(gaggle.Name), cfg, gaggle)
		if err != nil {
			// A relative workcopies.root: the daemon refuses to start on
			// this config, so no such directory is ever written. Naming a
			// path we could not resolve would be the drift this package
			// exists to prevent.
			continue
		}
		dirs = append(dirs, avexclusion.GaggleWorkcopiesDirectory(gaggle.Name, scoped))
	}
	return dirs
}

// workerAVExclusionDirectories is `goobers worker`'s set under workRoot,
// resolved through the same helpers workerEngineDepsForPlatform provisions
// with.
func workerAVExclusionDirectories(workRoot string, deps avExclusionDeps) []avexclusion.Directory {
	return avexclusion.WorkerDirectories(workRoot, workerWorkcopiesDir(workRoot), workerScratchDir(workRoot), deps.tempDir)
}

// stagePodAVExclusionAdvisory is the line a Windows stage pod prints to its
// own stderr before the stage runs — the far-side evidence #3480 asks for
// (`kubectl logs <pod>` names the excluded and not-excluded directories).
// The set is read from the pod's actual environment: the working directory
// the dispatcher stamped (the workspace mount), the TMP a tmp:ephemeral
// class bound, the container user's profile, the image's instance root.
// Empty off Windows. Never fails the stage: a hung or absent Defender is a
// reported unknown, bounded by avexclusion.DefenderQueryTimeout.
func stagePodAVExclusionAdvisory(ctx context.Context, deps avExclusionDeps, getwd func() (string, error), getenv func(string) string) string {
	if deps.hostOS != "windows" {
		return ""
	}
	workspace, err := getwd()
	if err != nil {
		workspace = dispatcher.WindowsWorkspacePath
	}
	tmp := firstNonEmpty(getenv("TMP"), getenv("TEMP"), deps.tempDir)
	home := firstNonEmpty(getenv("USERPROFILE"), getenv("HOME"))
	dirs := avexclusion.StagePodDirectories(workspace, tmp, home, getenv("GOOBERS_INSTANCE_ROOT"))
	return avexclusion.Summary("stage pod", avExclusionReport(ctx, dirs, deps))
}

// doctorAVExclusionsReport is the stable `--report json` shape of
// `goobers doctor --av-exclusions`.
type doctorAVExclusionsReport struct {
	// Host is the OS the command ran on; verification happened only when
	// it is "windows".
	Host string `json:"host"`
	// InstanceRoot is the instance root the daemon set was derived from.
	InstanceRoot string `json:"instanceRoot"`
	// WorkRoot is the worker work root the worker set was derived from.
	WorkRoot string `json:"workRoot"`
	avexclusion.Report
	// Runners lists every declared windows runner and its claim state, so
	// the declaration half (RNR006) is visible beside the verification half.
	Runners []doctorAVExclusionsRunner `json:"runners,omitempty"`
}

type doctorAVExclusionsRunner struct {
	Name string `json:"name"`
	// AVExclusionsVerified is the declared claim; nil when undeclared.
	AVExclusionsVerified *bool `json:"avExclusionsVerified"`
}

// runDoctorAVExclusions backs `goobers doctor --av-exclusions [--work-root
// <dir>] [instance-root]`. It enumerates the daemon set (from the instance
// root's Layout), the worker set (from --work-root, defaulting to the
// worker's own default), and the Windows stage-pod set (from the
// dispatcher's mount contract), then — on Windows — reads Defender's
// exclusion list and reports coverage per directory. ADVISORY: exit 0
// whatever the coverage; only a usage/IO error exits 2. The instance.yaml
// is loaded when present (for workcopies.root and the runners: inventory)
// but not required: the layout is pure path arithmetic.
func runDoctorAVExclusions(root, workRoot, reportFormat string, stdout, stderr io.Writer, deps avExclusionDeps) int {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		pf(stderr, "error: resolve instance root %q: %v\n", root, err)
		return 2
	}
	layout := instance.NewLayout(absRoot)
	var cfg *instance.Config
	if _, statErr := os.Stat(layout.ConfigFile()); statErr == nil {
		loaded, loadErr := instance.LoadConfig(layout.ConfigFile())
		if loadErr != nil {
			pf(stderr, "error: load config: %v\n", loadErr)
			return 2
		}
		cfg = loaded
	} else {
		pf(stderr, "note: %s not found; listing the default layout under %s\n", layout.ConfigFile(), absRoot)
	}
	// The config directory supplies the gaggle inventory, and with it every
	// per-gaggle workcopies.root override — directories the daemon writes
	// then reads that no other source names. Loaded FOR COMPARISON, not
	// fail-closed: this is diagnostic tooling, and an instance whose config
	// has an unrelated error is exactly an instance whose operator is about
	// to go looking at directories. When it cannot be loaded at all, say so
	// rather than report coverage over a set that is silently short.
	var set *instance.ConfigSet
	if _, statErr := os.Stat(layout.ConfigDir()); statErr == nil {
		loaded, configReport, loadErr := instance.LoadConfigDirForComparison(layout.ConfigDir())
		switch {
		case loaded != nil:
			set = loaded
			// The gaggles parsed, but the directory did not validate. Say
			// which findings, so an operator reading a coverage report knows
			// the inventory it was derived from is not one the daemon would
			// accept — rather than trusting a list silently built on it.
			if summary := validationIssueSummary(configReport); summary != "" {
				pf(stderr, "note: %s does not validate (%s); the gaggle roots below are read from it as-is\n", layout.ConfigDir(), summary)
			}
		case loadErr != nil:
			pf(stderr, "note: %s could not be loaded (%v); per-gaggle workcopies roots are NOT enumerated below\n", layout.ConfigDir(), loadErr)
		}
	}
	if workRoot == "" {
		workRoot = defaultWorkerRoot(deps.tempDir)
	}
	dirs := append(daemonAVExclusionDirectories(layout, cfg, set, deps), workerAVExclusionDirectories(workRoot, deps)...)
	dirs = append(dirs, avexclusion.StagePodDirectories(
		dispatcher.WindowsWorkspacePath, dispatcher.WindowsTmpPath, dispatcher.WindowsHomePath, "")...)

	report := doctorAVExclusionsReport{
		Host:         deps.hostOS,
		InstanceRoot: absRoot,
		WorkRoot:     workRoot,
		Report:       avExclusionReport(context.Background(), dirs, deps),
	}
	if cfg != nil {
		for _, entry := range cfg.Runners {
			if entry.Provides.OS != instance.RunnerOSWindows {
				continue
			}
			row := doctorAVExclusionsRunner{Name: entry.Name}
			if entry.Provides.Windows != nil {
				verified := entry.Provides.Windows.AVExclusionsVerified
				row.AVExclusionsVerified = &verified
			}
			report.Runners = append(report.Runners, row)
		}
	}

	if reportFormat == "json" {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			pf(stderr, "error: encode report: %v\n", err)
			return 2
		}
		return 0
	}
	writeDoctorAVExclusionsText(stdout, report)
	return 0
}

func writeDoctorAVExclusionsText(stdout io.Writer, report doctorAVExclusionsReport) {
	pf(stdout, "AV EXCLUSIONS (advisory, #3480): directories Goobers writes then immediately reads\n")
	pf(stdout, "  instance root: %s\n  worker work root: %s\n", report.InstanceRoot, report.WorkRoot)
	if report.Queried {
		pf(stdout, "  Microsoft Defender exclusions read: %d entr%s\n", len(report.Exclusions), plural(len(report.Exclusions), "y", "ies"))
	} else {
		pf(stdout, "  verification: unknown — %s\n", report.QueryError)
	}
	for _, f := range report.Findings {
		state := strings.ToUpper(string(f.Coverage))
		if f.MatchedBy != "" {
			state += " (by " + f.MatchedBy + ")"
		}
		pf(stdout, "  %-13s %-12s %s\n      %s\n", state, f.Role, f.Path, f.Purpose)
	}
	if len(report.Runners) > 0 {
		pf(stdout, "  declared windows runners (provides.windows.avExclusionsVerified — trusted, never verified):\n")
		for _, r := range report.Runners {
			claim := "undeclared (RNR006)"
			switch {
			case r.AVExclusionsVerified == nil:
			case *r.AVExclusionsVerified:
				claim = "true"
			default:
				claim = "false (RNR006)"
			}
			pf(stdout, "    %s: %s\n", r.Name, claim)
		}
	}
	pf(stdout, "  Nothing here changes host antivirus settings; see docs/guides/windows-large-repo-runbook.md.\n")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
