// Package avexclusion names the directories Goobers writes and immediately
// reads back, and verifies — advisorily — whether Microsoft Defender's
// real-time scanning excludes them (#3480).
//
// On Windows, real-time antivirus scanning holds a handle on a freshly
// created file for a brief window. A `git commit` in a just-provisioned
// workcopy, a `.git/config` written and re-read moments later, a harness log
// flushed and tailed — each can lose that race, and the failure surfaces
// minutes later, in a different subsystem, as an unrelated
// `Permission denied` from git (#3161–#3164). Nothing in the product told
// the operator which directories to exclude or whether the ones in use were
// excluded. This package is that missing declaration and check.
//
// Two rules keep it honest:
//
//   - The directory set is DERIVED from the same path code the daemon, the
//     worker and the stage pod actually use (instance.Layout, the worker's
//     work-root layout, the dispatcher's mount contract), never retyped —
//     so the list an operator feeds to their tooling cannot drift from what
//     the binary writes.
//   - Verification is ADVISORY, never fail-closed (the issue's own scope
//     note). An operator may already run an organisation-wide AV policy
//     Goobers must not fight; the product's job is to make the gap visible,
//     not to refuse to start. Nothing here mutates host AV configuration.
package avexclusion

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/instance"
)

// Role names which Goobers process a directory belongs to. One host can play
// several (a developer laptop runs the daemon and, through `goobers run`, is
// also the stage substrate), and an operator excluding paths wants to know
// which process each one serves.
type Role string

const (
	// RoleDaemon is the instance-root owner: `goobers up` / `goobers run`,
	// the `self` runner in a runners: inventory.
	RoleDaemon Role = "daemon"
	// RoleWorker is `goobers worker`, whose --work-root holds the stage
	// workspaces it provisions for in-process (mode-2) stage execution.
	RoleWorker Role = "worker"
	// RoleStagePod is a dispatcher-created Windows stage pod (mode 3): the
	// workspace mount, the profile-nested temp, the container user's home.
	RoleStagePod Role = "stage-pod"
)

// Directory is one path Goobers writes then reads, with the role that owns
// it and why it is on the list.
type Directory struct {
	Role    Role   `json:"role"`
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}

// Coverage is the verification outcome for one directory.
type Coverage string

const (
	// CoverageExcluded means an exclusion entry covers the directory (the
	// entry names it, an ancestor, or a wildcard pattern matching either).
	CoverageExcluded Coverage = "excluded"
	// CoverageNotExcluded means the exclusion list was read and nothing in
	// it covers the directory.
	CoverageNotExcluded Coverage = "not-excluded"
	// CoverageUnknown means the exclusion list could not be read at all —
	// not a Windows host, PowerShell or Defender unavailable, query failed.
	CoverageUnknown Coverage = "unknown"
)

// Finding is one directory's verification outcome.
type Finding struct {
	Directory
	Coverage Coverage `json:"coverage"`
	// MatchedBy is the exclusion entry that covers the directory, when one
	// does — so an operator can see WHICH rule is doing the work.
	MatchedBy string `json:"matchedBy,omitempty"`
}

// Report is the advisory result for a directory set.
type Report struct {
	// Queried is true when the host's Defender exclusion list was read.
	Queried bool `json:"queried"`
	// QueryError explains why it was not, when it was not.
	QueryError string `json:"queryError,omitempty"`
	// Exclusions is the exclusion list as read (environment variables
	// expanded), when queried.
	Exclusions []string  `json:"exclusions,omitempty"`
	Findings   []Finding `json:"findings"`
}

// DaemonDirectories is the instance-root owner's set, derived from layout —
// the same Layout the daemon resolves its journals, ledger, blob store and
// workcopies through. tempDir is the process temp directory (os.TempDir):
// the executor's scratch workspaces and every harness's own temp land there
// unless a stage redirects them.
//
// The instance root covers most of the others by subtree, but each is listed
// on its own: an operator who relocates workcopies.root outside the instance
// root, or excludes the root and forgets TEMP, should see exactly which
// entry is uncovered rather than a single coarse verdict.
func DaemonDirectories(layout instance.Layout, tempDir string) []Directory {
	dirs := []Directory{
		{Role: RoleDaemon, Path: layout.Root, Purpose: "instance root (instance.yaml, config tree, telemetry/read/intake databases)"},
		{Role: RoleDaemon, Path: layout.RunsDir(), Purpose: "run journals (events.jsonl, per-stage artifacts, written and re-read within one stage)"},
		{Role: RoleDaemon, Path: layout.GagglesDir(), Purpose: "per-gaggle runtime state (run journals and default workcopies of every gaggle)"},
		{Role: RoleDaemon, Path: layout.SchedulerDir(), Purpose: "scheduler journal, claim ledger and durable stage cursors"},
		{Role: RoleDaemon, Path: layout.BlobStoreDir(), Purpose: "content-addressed blob store (stage artifacts and workspace deltas, PUT then GET)"},
		{Role: RoleDaemon, Path: layout.WorkcopiesBaseDir(), Purpose: "managed working copies (git mirrors and per-run worktrees; the harness sandbox home, log and temp directories live inside each workspace)"},
	}
	if tempDir != "" {
		dirs = append(dirs, Directory{Role: RoleDaemon, Path: tempDir, Purpose: "process temp directory (TMP/TEMP): scratch workspaces and harness temp files"})
	}
	return dirs
}

// GaggleWorkcopiesDirectory names one gaggle's managed working copies, from
// the layout the daemon itself resolves for that gaggle.
//
// This entry is NOT redundant with DaemonDirectories' workcopies base. A
// gaggle may set `spec.workcopies.root` to any absolute path, and that
// override WINS over the instance-wide `workcopies.root`
// (instance.EffectiveWorkcopiesLayout); the daemon applies it per gaggle
// when it builds that gaggle's worktree.Manager. The result holds the git
// mirrors and per-run worktrees — the write-then-read hot spot #3161–#3164
// describes. Enumerating only the instance-wide root would leave a gaggle
// pointed at another drive neither named nor judged, and Summary would then
// print an affirmative all-clear over a directory it never looked at: the
// worst failure mode an advisory has.
//
// scoped is that gaggle's own EffectiveWorkcopiesLayout. Dedupe collapses
// the common case, where the gaggle inherits the instance root.
func GaggleWorkcopiesDirectory(gaggle string, scoped instance.Layout) Directory {
	return Directory{
		Role: RoleDaemon,
		Path: scoped.WorkcopiesBaseDir(),
		Purpose: fmt.Sprintf("gaggle %q managed working copies (git mirrors and per-run worktrees, at workcopies.root as the daemon resolves it for this gaggle)",
			gaggle),
	}
}

// WorkerDirectories is `goobers worker`'s set. workcopies and scratch are
// the two subtrees the worker provisions under its work root (the caller
// passes the worker's own resolution of them, so the names are never
// retyped here). tempDir as for DaemonDirectories.
func WorkerDirectories(workRoot, workcopies, scratch, tempDir string) []Directory {
	dirs := []Directory{
		{Role: RoleWorker, Path: workRoot, Purpose: "worker work root (--work-root)"},
		{Role: RoleWorker, Path: workcopies, Purpose: "worker managed working copies (git mirrors and per-stage worktrees)"},
		{Role: RoleWorker, Path: scratch, Purpose: "worker scratch workspaces"},
	}
	if tempDir != "" {
		dirs = append(dirs, Directory{Role: RoleWorker, Path: tempDir, Purpose: "process temp directory (TMP/TEMP)"})
	}
	return dirs
}

// StagePodDirectories is a Windows stage pod's set: the workspace the
// dispatcher mounts and runs the stage in (checkout, commits, result file),
// the temp path a tmp:ephemeral runner class binds TMP/TEMP to, the
// container user's profile (git and harness config, credential helpers'
// scratch), and the image's instance root when one is stamped. Empty values
// are omitted so the caller can pass whatever the environment resolves.
func StagePodDirectories(workspace, tmp, home, instanceRoot string) []Directory {
	var dirs []Directory
	add := func(path, purpose string) {
		if strings.TrimSpace(path) == "" {
			return
		}
		dirs = append(dirs, Directory{Role: RoleStagePod, Path: path, Purpose: purpose})
	}
	add(workspace, "stage workspace mount (checkout, stage commits, result file)")
	add(tmp, "stage temp directory (TMP/TEMP; the tmp:ephemeral mount)")
	add(home, "container user profile (git and harness configuration)")
	add(instanceRoot, "image instance root (GOOBERS_INSTANCE_ROOT)")
	return dirs
}

// Verify matches every directory against the exclusion list. queried=false
// (the list could not be read) marks every directory CoverageUnknown and
// records why; the directories are still reported, because the LIST is the
// deliverable an operator without a readable Defender still needs.
func Verify(dirs []Directory, exclusions []string, queried bool, queryErr error) Report {
	report := Report{Queried: queried, Findings: make([]Finding, 0, len(dirs))}
	if !queried {
		if queryErr != nil {
			report.QueryError = queryErr.Error()
		} else {
			report.QueryError = "exclusion list not queried"
		}
		for _, dir := range dirs {
			report.Findings = append(report.Findings, Finding{Directory: dir, Coverage: CoverageUnknown})
		}
		return report
	}
	report.Exclusions = append([]string(nil), exclusions...)
	for _, dir := range dirs {
		finding := Finding{Directory: dir, Coverage: CoverageNotExcluded}
		if by, ok := Covers(exclusions, dir.Path); ok {
			finding.Coverage = CoverageExcluded
			finding.MatchedBy = by
		}
		report.Findings = append(report.Findings, finding)
	}
	return report
}

// Covers reports whether any exclusion entry covers path, and which. An
// entry covers a path when, after normalisation (forward slashes to
// backslashes, trailing separators trimmed, case-insensitive — Defender's
// own comparison is case-insensitive), it equals the path, names one of its
// ancestors, or is a wildcard pattern (`*`, `?`, Defender's two) matching
// the path or one of its ancestors.
//
// The wildcard arm APPROXIMATES Defender, and does so deliberately in the
// conservative direction — it may report not-excluded for something
// Defender excludes, never excluded for something Defender scans:
//
//   - `*` and `?` stand for characters WITHIN one path component and never
//     span a separator, matching Defender's documented folder-exclusion
//     rule that `*` replaces a single folder level (nested levels need one
//     `*` per level). Letting `*` span separators would make
//     `C:\Users\*\AppData\Local\Temp` report EXCLUDED for
//     `C:\Users\a\b\c\AppData\Local\Temp` — a false all-clear.
//   - An entry with a trailing `\*` does NOT cover the directory itself:
//     `C:\workspace\*` leaves `C:\workspace` reported not-excluded even
//     though an operator plainly meant the subtree. That is a spurious
//     warning, which costs an operator a second look; the opposite error
//     costs them the race this whole package exists to name.
func Covers(exclusions []string, path string) (string, bool) {
	target := normalise(path)
	if target == "" {
		return "", false
	}
	for _, raw := range exclusions {
		entry := normalise(raw)
		if entry == "" {
			continue
		}
		for candidate := target; candidate != ""; candidate = parent(candidate) {
			if strings.EqualFold(candidate, entry) || wildcardMatch(entry, candidate) {
				return raw, true
			}
		}
	}
	return "", false
}

// normalise puts a Windows path into the shape Covers compares: backslash
// separators, no trailing separator, no surrounding whitespace. Case is
// left alone; comparison is case-insensitive.
func normalise(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "/", `\`)
	for len(p) > 3 && strings.HasSuffix(p, `\`) {
		p = strings.TrimSuffix(p, `\`)
	}
	return p
}

// parent returns the parent of a normalised path, or "" at the root. It is
// deliberately not filepath.Dir: this code runs on the operator's laptop
// (macOS/Linux, when `goobers doctor --av-exclusions` merely lists) as well
// as on Windows, and filepath.Dir would not split on backslashes there.
func parent(p string) string {
	i := strings.LastIndex(p, `\`)
	if i <= 0 {
		return ""
	}
	// `C:\foo` -> `C:\` (a whole-drive exclusion is a real, if coarse,
	// entry); `C:\` itself is the root and has no parent.
	if i == 2 && p[1] == ':' {
		if len(p) > 3 {
			return p[:3]
		}
		return ""
	}
	return p[:i]
}

// pathSeparator is the separator normalise leaves behind, and the boundary
// neither wildcard crosses.
const pathSeparator = '\\'

// wildcardMatch matches s against a Defender-style pattern where `*` spans
// any run of characters WITHIN one path component and `?` exactly one such
// character, case-insensitively. Neither crosses a separator: Defender's
// folder wildcard stands for a single folder level, so
// `C:\Users\*\AppData` covers `C:\Users\alice\AppData` and not
// `C:\Users\alice\bob\AppData` (see Covers' doc comment for why the error
// this prevents is the dangerous direction). Deeper nesting is expressed
// the way Defender expresses it, one `*` per level.
//
// Only patterns that actually contain a wildcard are matched this way; a
// literal entry is handled by the equality arm of Covers, and Covers' walk
// up the ancestors is what makes a matched folder cover its whole subtree.
func wildcardMatch(pattern, s string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return false
	}
	p := []rune(strings.ToLower(pattern))
	t := []rune(strings.ToLower(s))
	// Classic two-pointer glob with backtracking on the last `*`.
	pi, ti := 0, 0
	star, mark := -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star = pi
			mark = ti
			pi++
		case pi < len(p) && ((p[pi] == '?' && t[ti] != pathSeparator) || p[pi] == t[ti]):
			pi++
			ti++
		// Backtracking widens the last `*` by one more character of s —
		// t[mark]. A separator is not a character `*` may absorb, so it
		// ends the backtrack rather than being swallowed.
		case star >= 0 && mark < len(t) && t[mark] != pathSeparator:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// Summary renders the report as ONE line for a process log — the shape the
// pod entrypoint, the daemon and the worker print once at startup so the
// far-side evidence (`kubectl logs`, the daemon log) names the directories
// and their coverage without a second command. label names the process
// ("stage pod", "daemon", "worker").
func Summary(label string, report Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "av-exclusions (advisory, %s): ", label)
	if !report.Queried {
		fmt.Fprintf(&b, "could not read Microsoft Defender exclusions (%s); directories Goobers writes then reads: %s",
			report.QueryError, joinPaths(report.Findings, func(Finding) bool { return true }))
		return b.String()
	}
	excluded := 0
	for _, f := range report.Findings {
		if f.Coverage == CoverageExcluded {
			excluded++
		}
	}
	fmt.Fprintf(&b, "%d of %d directories Goobers writes then reads are excluded from real-time scanning", excluded, len(report.Findings))
	if missing := joinPaths(report.Findings, func(f Finding) bool { return f.Coverage == CoverageNotExcluded }); missing != "" {
		fmt.Fprintf(&b, "; NOT excluded: %s — a scan holding a handle on a just-written file can surface later as an unrelated git \"Permission denied\" (#3480); see docs/guides/windows-large-repo-runbook.md", missing)
	} else {
		b.WriteString("; every enumerated directory is covered")
	}
	return b.String()
}

func joinPaths(findings []Finding, keep func(Finding) bool) string {
	var parts []string
	for _, f := range findings {
		if keep(f) {
			parts = append(parts, fmt.Sprintf("%s (%s)", f.Path, f.Purpose))
		}
	}
	return strings.Join(parts, ", ")
}

// Dedupe drops directories whose normalised path repeats an earlier one,
// keeping first-seen order — a worker whose --work-root IS the temp
// directory, or a layout whose gaggle-less RunsDir sits under the root it
// already listed, should not print the same path twice.
func Dedupe(dirs []Directory) []Directory {
	seen := make(map[string]bool, len(dirs))
	out := make([]Directory, 0, len(dirs))
	for _, dir := range dirs {
		key := strings.ToLower(normalise(dir.Path))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, dir)
	}
	return out
}
