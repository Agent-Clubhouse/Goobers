// Package instance implements the tier 1-2 local instance root: the on-disk
// layout `goobers init` scaffolds, the instance.yaml provisioning file, and
// the config/ directory loader (ARCHITECTURE.md §6, INST-010/011/012).
package instance

import (
	"path/filepath"
	"strings"
)

// Layout names for the pieces of an instance root (ARCHITECTURE.md §6).
const (
	ConfigDirName     = "config"
	GagglesDirName    = "gaggles"
	RunsDirName       = "runs"
	SchedulerDirName  = "scheduler"
	WorkcopiesDirName = "workcopies"
	TelemetryDBName   = "telemetry.db"
	ReadDBName        = "read.db"
	ConfigFileName    = "instance.yaml"
	// DocsUpdaterDirName is the SchedulerDir subdirectory holding the
	// docs-updater workflow's per-workflow durable state — the docs-drift
	// watermark (#1015). It lives under the instance-wide scheduler dir, not
	// under a per-run directory, precisely because the watermark must outlive
	// any single run so successive runs advance from where the last left off.
	DocsUpdaterDirName = "docs-updater"
	// TutorHoldoutsDirName holds cross-run Tutor finding state. A Tutor run
	// opens a config PR before promotion, so its mandatory live-verification
	// finding must outlive that authoring run.
	TutorHoldoutsDirName = "tutor-holdouts"
	// BacklogHealthDirName is the SchedulerDir subdirectory holding the
	// backlog-health stage's durable ready-transition ledger and its
	// provider-event high-water mark (#3392). Like the docs watermark it is
	// instance-wide rather than per-run precisely because its whole purpose is
	// to let the next cycle resume instead of re-reading the repo's entire
	// issue-event history.
	BacklogHealthDirName = "backlog-health"
)

// Layout resolves the paths that make up an instance root.
type Layout struct {
	// Root is the instance root directory.
	Root string

	gaggle         string
	workcopiesRoot string
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

// WithWorkcopiesRoot redirects managed working copies to root. The configured
// root is a base: gaggle-scoped layouts append their gaggle name so separate
// workforces cannot accidentally share mutable worktrees.
func (l Layout) WithWorkcopiesRoot(root string) Layout {
	l.workcopiesRoot = root
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
	if l.workcopiesRoot != "" {
		if l.gaggle == "" {
			return l.workcopiesRoot
		}
		return filepath.Join(l.workcopiesRoot, l.gaggle)
	}
	return filepath.Join(l.runtimeRoot(), WorkcopiesDirName)
}

// WorkcopiesBaseDir is the base used by pinned workspaces. The legacy default
// remains instance-scoped; an alternate root retains its gaggle segment so a
// configured short path cannot make two gaggles share mutable state.
func (l Layout) WorkcopiesBaseDir() string {
	if l.workcopiesRoot != "" {
		return l.WorkcopiesDir()
	}
	return filepath.Join(l.Root, WorkcopiesDirName)
}

// TelemetryDB is the path to the local telemetry rollup store (§8).
func (l Layout) TelemetryDB() string { return filepath.Join(l.Root, TelemetryDBName) }

// ReadDB is the path to the portal's run read model (read.db).
//
// A separate file from telemetry.db on purpose. The read model is gated on
// 191 MB of run events while telemetry.db is 547 MB gated on 2,263 MB of spans,
// so keeping them apart means cold start pays for the data the product IS rather
// than the data that decorates it, and the two get the independent retention
// policies they already need (design docs/design/portal-read-architecture.md
// §5.1).
func (l Layout) ReadDB() string { return filepath.Join(l.Root, ReadDBName) }

// IntakeDBName is the source-watermark database (#1922).
//
// Separate from read.db, and never rebuilt. Out-of-process writers record
// watermarks here, so an epoch swap that replaced this file would lose the
// watermarks of any process still holding the old inode — and on Windows could
// not replace it at all while one did.
const IntakeDBName = "intake.db"

// IntakeDB returns the source-watermark database path.
func (l Layout) IntakeDB() string { return filepath.Join(l.Root, IntakeDBName) }

// DocsWatermarkPath returns the durable docs-drift watermark file for a
// (gaggle, workflow) pair (#1015). The watermark records the commit the
// docs-updater last refreshed docs against; the signal-gather stage reads it to
// bound its churn window and advances it on a successful pass. It lives under
// the instance-wide SchedulerDir (like the claim ledger) rather than a per-run
// dir so it persists across runs. gaggle may be empty (the legacy flat layout);
// both segments are name-sanitized so the file name stays a single, safe path
// component.
func (l Layout) DocsWatermarkPath(gaggle, workflow string) string {
	name := docsWatermarkSegment(gaggle) + "__" + docsWatermarkSegment(workflow) + ".json"
	return filepath.Join(l.SchedulerDir(), DocsUpdaterDirName, name)
}

// TutorHoldoutsDir is the durable scheduler-side store for post-promotion
// Tutor live-verification findings.
func (l Layout) TutorHoldoutsDir() string {
	return filepath.Join(l.SchedulerDir(), TutorHoldoutsDirName)
}

// TutorHoldoutPath returns the durable file for one Tutor authoring run.
// Repasses overwrite this same path atomically, so replacing a finding never
// creates a delete-before-write gap.
func (l Layout) TutorHoldoutPath(gaggle, runID string) string {
	name := tutorHoldoutSegment(gaggle) + "__" + tutorHoldoutSegment(runID) + ".json"
	return filepath.Join(l.TutorHoldoutsDir(), name)
}

// BacklogHealthCursorPath returns the durable ready-transition cursor file for
// one (gaggle, provider, repository, label) scan (#3392). repository is the
// provider-native "owner/name" key; label is the ready label whose transitions
// the ledger holds. Every segment is name-sanitized (labels carry a ":") so the
// file name stays a single, safe path component.
func (l Layout) BacklogHealthCursorPath(gaggle, provider, repository, label string) string {
	name := strings.Join([]string{
		tutorHoldoutSegment(gaggle),
		tutorHoldoutSegment(provider),
		tutorHoldoutSegment(repository),
		tutorHoldoutSegment(label),
	}, "__") + ".json"
	return filepath.Join(l.SchedulerDir(), BacklogHealthDirName, name)
}

func tutorHoldoutSegment(s string) string {
	return strings.NewReplacer(":", "_").Replace(docsWatermarkSegment(s))
}

// docsWatermarkSegment reduces a gaggle/workflow name to a safe single file-name
// segment. Config object names are already restricted to lowercase alphanumerics
// and hyphens, so this only has to defend the empty and stray-separator cases
// so a bad env var can never redirect the watermark outside its directory.
func docsWatermarkSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	replacer := strings.NewReplacer("/", "_", `\`, "_", ".", "_", " ", "_")
	return replacer.Replace(s)
}
