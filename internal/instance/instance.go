// Package instance implements the tier 1-2 local instance root: the on-disk
// layout `goobers init` scaffolds, the instance.yaml provisioning file, and
// the config/ directory loader (ARCHITECTURE.md §6, INST-010/011/012).
package instance

import "path/filepath"

// Layout names for the pieces of an instance root (ARCHITECTURE.md §6).
const (
	ConfigDirName     = "config"
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
}

// NewLayout returns the Layout rooted at root.
func NewLayout(root string) Layout {
	return Layout{Root: root}
}

// ConfigFile is the path to instance.yaml.
func (l Layout) ConfigFile() string { return filepath.Join(l.Root, ConfigFileName) }

// ConfigDir is the path to the config-as-code directory (gaggles, goobers,
// workflows, gates) — the only path the Tutor may write to (INST-014).
func (l Layout) ConfigDir() string { return filepath.Join(l.Root, ConfigDirName) }

// RunsDir is the path to the run journals directory (§4).
func (l Layout) RunsDir() string { return filepath.Join(l.Root, RunsDirName) }

// SchedulerDir is the path to the instance journal (scheduler decisions +
// claim ledger, §4/§7).
func (l Layout) SchedulerDir() string { return filepath.Join(l.Root, SchedulerDirName) }

// WorkcopiesDir is the path to managed working copies of target repos.
func (l Layout) WorkcopiesDir() string { return filepath.Join(l.Root, WorkcopiesDirName) }

// TelemetryDB is the path to the local telemetry rollup store (§8).
func (l Layout) TelemetryDB() string { return filepath.Join(l.Root, TelemetryDBName) }
