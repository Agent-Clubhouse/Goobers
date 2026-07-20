// Package instance implements the tier 1-2 local instance root: the on-disk
// layout `goobers init` scaffolds, the instance.yaml provisioning file, and
// the config/ directory loader (ARCHITECTURE.md §6, INST-010/011/012).
package instance

import "path/filepath"

// Layout names for the pieces of an instance root (ARCHITECTURE.md §6).
const (
	ConfigDirName     = "config"
	GagglesDirName    = "gaggles"
	RunsDirName       = "runs"
	SchedulerDirName  = "scheduler"
	WorkcopiesDirName = "workcopies"
	TelemetryDBName   = "telemetry.db"
	ConfigFileName    = "instance.yaml"
)

// Layout resolves the paths that make up an instance root.
type Layout struct {
	// Root is the instance root directory.
	Root string

	gaggle string
}

// NewLayout returns the Layout rooted at root.
func NewLayout(root string) Layout {
	return Layout{Root: root}
}

// ForGaggle returns the runtime-scoped layout for gaggle. Instance-wide paths
// such as config, scheduler, and telemetry remain rooted at the instance.
func (l Layout) ForGaggle(gaggle string) Layout {
	l.gaggle = gaggle
	return l
}

// Gaggle returns the runtime scope, or empty for the legacy flat layout.
func (l Layout) Gaggle() string { return l.gaggle }

// ConfigFile is the path to instance.yaml.
func (l Layout) ConfigFile() string { return filepath.Join(l.Root, ConfigFileName) }

// ConfigDir is the path to the config-as-code directory (gaggles, goobers,
// workflows, gates) — the only path the Tutor may write to (INST-014).
func (l Layout) ConfigDir() string { return filepath.Join(l.Root, ConfigDirName) }

// GagglesDir is the parent of all per-gaggle runtime state.
func (l Layout) GagglesDir() string { return filepath.Join(l.Root, GagglesDirName) }

func (l Layout) runtimeRoot() string {
	if l.gaggle == "" {
		return l.Root
	}
	return filepath.Join(l.GagglesDir(), l.gaggle)
}

// RunsDir is the path to this layout's run journals directory (§4).
func (l Layout) RunsDir() string { return filepath.Join(l.runtimeRoot(), RunsDirName) }

// SchedulerDir is the path to the instance journal (scheduler decisions +
// claim ledger, §4/§7).
func (l Layout) SchedulerDir() string { return filepath.Join(l.Root, SchedulerDirName) }

// WorkcopiesDir is the path to this layout's managed working copies.
func (l Layout) WorkcopiesDir() string {
	return filepath.Join(l.runtimeRoot(), WorkcopiesDirName)
}

// TelemetryDB is the path to the local telemetry rollup store (§8).
func (l Layout) TelemetryDB() string { return filepath.Join(l.Root, TelemetryDBName) }
