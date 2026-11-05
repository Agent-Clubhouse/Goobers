package runtimewiring

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/goobers/goobers/internal/avexclusion"
)

// DefaultWorkerRoot is the fallback instance directory inside a worker container.
func DefaultWorkerRoot(tempDir string) string { return filepath.Join(tempDir, "goobers-worker") }

// WorkerWorkcopiesDir names the worker-owned checkout directory.
func WorkerWorkcopiesDir(workRoot string) string { return filepath.Join(workRoot, "workcopies") }

// WorkerScratchDir names the worker scratch directory.
func WorkerScratchDir(workRoot string) string { return filepath.Join(workRoot, "scratch") }

// AvExclusionDeps is the seam the advisory runs through: the host OS
// (verification is Windows-only; elsewhere the set is listed and coverage
// reported unknown), the exclusion-list reader, and the process temp
// directory.
type AvExclusionDeps struct {
	HostOS  string
	Query   avexclusion.Querier
	TempDir string
}

// RealAVExclusionDeps binds the antivirus advisory to the host operating system.
func RealAVExclusionDeps() AvExclusionDeps {
	return AvExclusionDeps{HostOS: runtime.GOOS, Query: avexclusion.QueryDefender, TempDir: filepath.Clean(os.TempDir())}
}

// AvExclusionReport verifies dirs against the host's exclusion list. Off
// Windows there is nothing to read: every directory is reported unknown with
// the reason, so a `doctor --av-exclusions` run on an operator's macOS
// laptop still yields the list.
func AvExclusionReport(ctx context.Context, dirs []avexclusion.Directory, deps AvExclusionDeps) avexclusion.Report {
	dirs = avexclusion.Dedupe(dirs)
	if deps.HostOS != "windows" {
		return avexclusion.Verify(dirs, nil, false, fmt.Errorf("verification runs on Windows only (this host is %s)", deps.HostOS))
	}
	if deps.Query == nil {
		return avexclusion.Verify(dirs, nil, false, fmt.Errorf("no exclusion-list reader configured"))
	}
	exclusions, err := deps.Query(ctx)
	if err != nil {
		return avexclusion.Verify(dirs, nil, false, err)
	}
	return avexclusion.Verify(dirs, exclusions, true, nil)
}

// HostAVExclusionAdvisory is the one-line startup advisory the daemon and
// the worker print. Empty off Windows: a Linux daemon has no AV race to
// warn about, and the existing large-repo preflight keeps the same silence.
func HostAVExclusionAdvisory(ctx context.Context, label string, dirs []avexclusion.Directory, deps AvExclusionDeps) string {
	if deps.HostOS != "windows" {
		return ""
	}
	return avexclusion.Summary(label, AvExclusionReport(ctx, dirs, deps))
}

// WorkerAVExclusionDirectories is `goobers-cloud worker`'s set under workRoot,
// resolved through the same helpers workerEngineDepsForPlatform provisions
// with.
func WorkerAVExclusionDirectories(workRoot string, deps AvExclusionDeps) []avexclusion.Directory {
	return avexclusion.WorkerDirectories(workRoot, WorkerWorkcopiesDir(workRoot), WorkerScratchDir(workRoot), deps.TempDir)
}
