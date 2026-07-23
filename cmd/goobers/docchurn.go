package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/configboundary"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/platform/durability"
)

// docs-churn is the docs-updater workflow's deterministic signal-gather stage
// (#1015, epic #472). On each wake it reports the code churn a docs goober must
// reason over — the commits and changed files since docs were last refreshed,
// widened by a buffer so nothing at the boundary is missed — and nothing else:
// it writes no docs and opens no PR (that is the gated capstone #1018). It is
// the churn-based analog of tutor.yaml's telemetry-query connector stage.
//
// Watermark + buffer. A durable per-(gaggle,workflow) watermark
// (instance.Layout.DocsWatermarkPath) records the commit docs were last
// refreshed against. The window this stage reports is [watermark −
// buffer, HEAD], where buffer = max(bufferMultiplier × time-since-last-run,
// sinceFloor). The overlap back past the watermark is deliberate: re-running
// over already-documented churn is a no-op for the goober (it dedupes), and the
// overlap is what guarantees a boundary commit is never dropped. The first run
// (no watermark) falls back to a bounded [now − sinceFloor, HEAD] window rather
// than all of history.
//
// The watermark advances to HEAD on a successful pass (advanceWatermark, default
// true), so successive runs start from where the last left off — no
// re-processing from scratch, and no gap. Advancing here, at gather time, is
// safe even before the capstone wires a docs-writing stage after it precisely
// because of the buffer: were a future downstream stage to fail after the
// watermark advanced, the next run's buffer window still reaches back across the
// advanced watermark and re-surfaces the churn. A caller that instead wants the
// watermark advanced only by a terminal stage of its own can set
// advanceWatermark=false and advance it itself; that terminal stage is out of
// scope for this foundation.
const (
	docsChurnSchemaVersion   = "goobers.dev/docs-churn/v1"
	docsChurnFormat          = "churn-digest"
	docsChurnDefaultFloor    = 168 * time.Hour
	docsChurnDefaultBuffer   = 3.0
	docsChurnNoChurnNote     = "no code churn in the reported window"
	docsChurnFirstRunNote    = "first run: no watermark yet, bounded to the since-floor window"
	docsChurnEmptyTreeObject = "4b825dc642cb6eb9a060e54bf8d69288fbee4904" // git's well-known empty tree
)

// docsWatermark is the durable last-refreshed marker persisted between runs.
type docsWatermark struct {
	Schema      string    `json:"schema"`
	Gaggle      string    `json:"gaggle,omitempty"`
	Workflow    string    `json:"workflow"`
	SHA         string    `json:"sha"`
	RefreshedAt time.Time `json:"refreshedAt"`
}

const docsWatermarkSchemaVersion = "goobers.dev/docs-watermark/v1"

type churnCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Body    string `json:"body,omitempty"`
}

type docsChurnDigest struct {
	Schema           string              `json:"schema"`
	FirstRun         bool                `json:"firstRun"`
	Since            time.Time           `json:"since"`
	Head             string              `json:"head"`
	Base             string              `json:"base,omitempty"`
	Watermark        *docsWatermark      `json:"watermark,omitempty"`
	BufferMultiplier float64             `json:"bufferMultiplier"`
	SinceFloor       string              `json:"sinceFloor"`
	CommitCount      int                 `json:"commitCount"`
	Commits          []churnCommit       `json:"commits"`
	ChangedFiles     []string            `json:"changedFiles"`
	Areas            map[string][]string `json:"areas"`
	DocsRoots        []string            `json:"docsRoots,omitempty"`
	DocsRootChanges  []string            `json:"docsRootChanges,omitempty"`
	NoWork           bool                `json:"noWork,omitempty"`
	Note             string              `json:"note,omitempty"`
}

const docsChurnHelp = "Usage: goobers docs-churn [--repo <dir>] [--workflow <name>] [--gaggle <name>] " +
	"[--since <duration>] [--buffer-multiplier <float>] [--format churn-digest] [path]\n\n" +
	"Report the code churn since docs were last refreshed for the docs-updater\n" +
	"workflow (#1015). Reads and advances a durable per-(gaggle,workflow)\n" +
	"watermark under the instance's scheduler dir, and writes a versioned\n" +
	"churn-digest to GOOBERS_INPUT_resultFile when declared, else stdout.\n" +
	"[path] is the instance root (default $GOOBERS_INSTANCE_ROOT, else \".\").\n\n" +
	"Exit codes: 0 = OK (including a clean no-work result), 1 = business error,\n" +
	"2 = usage/IO error.\n"

func runDocsChurn(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("docs-churn", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "git repository/worktree to scan (default the stage's worktree)")
	workflowFlag := fs.String("workflow", "", "workflow name keying the watermark (default $GOOBERS_WORKFLOW)")
	gaggleFlag := fs.String("gaggle", "", "gaggle name keying the watermark (default $GOOBERS_GAGGLE)")
	since := fs.Duration("since", docsChurnDefaultFloor, "first-run window and minimum buffer floor (e.g. 168h)")
	bufferMultiplier := fs.Float64("buffer-multiplier", docsChurnDefaultBuffer,
		"multiply the time-since-last-run span to extend the window back past the watermark (>= 1)")
	format := fs.String("format", docsChurnFormat, "artifact format (churn-digest)")
	fs.Usage = helpUsage(stderr, "docs-churn")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *since <= 0 {
		pf(stderr, "error: --since must be a positive duration, got %s\n", *since)
		return 2
	}
	if *bufferMultiplier < 1 {
		pf(stderr, "error: --buffer-multiplier must be >= 1 (a smaller window than since-last-run could miss churn), got %v\n", *bufferMultiplier)
		return 2
	}
	if *format != docsChurnFormat {
		pf(stderr, "error: --format must be %q, got %q\n", docsChurnFormat, *format)
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}

	// Inputs a workflow node wires (executor.InputEnvVar); flags win for
	// standalone use. sinceFloor/bufferMultiplier mirror the flag defaults so a
	// node can tune them without a bespoke command line.
	floor := *since
	if v := providerInput("sinceFloor", ""); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			pf(stderr, "error: input sinceFloor must be a positive duration, got %q\n", v)
			return 2
		}
		floor = d
	}
	multiplier := *bufferMultiplier
	if v := providerInput("bufferMultiplier", ""); v != "" {
		m, err := strconv.ParseFloat(v, 64)
		if err != nil || m < 1 {
			pf(stderr, "error: input bufferMultiplier must be a number >= 1, got %q\n", v)
			return 2
		}
		multiplier = m
	}

	workflow := firstNonEmpty(*workflowFlag, os.Getenv("GOOBERS_WORKFLOW"))
	if workflow == "" {
		pf(stderr, "error: --workflow or $GOOBERS_WORKFLOW is required (the docs watermark is per-workflow)\n")
		return 2
	}
	gaggle := firstNonEmpty(*gaggleFlag, providerGaggle())
	docsRoots := parseDocsRoots(providerInput("docsRoots", ""))
	advance := providerInput("advanceWatermark", "true") == "true"

	root := providerStageRoot(pathArg)
	wmPath := layoutFor(root).DocsWatermarkPath(gaggle, workflow)
	watermark, haveWatermark, err := readDocsWatermark(wmPath)
	if err != nil {
		pf(stderr, "error: read docs watermark %s: %v\n", wmPath, err)
		return 1
	}

	head, err := gitRevParse(*repo, "HEAD")
	if err != nil {
		pf(stderr, "error: resolve HEAD in %s: %v\n", *repo, err)
		return 1
	}

	now := time.Now().UTC()
	firstRun := !haveWatermark
	var sinceTime time.Time
	if firstRun {
		sinceTime = now.Add(-floor)
	} else {
		sinceLast := now.Sub(watermark.RefreshedAt)
		if sinceLast < 0 {
			sinceLast = 0
		}
		buffer := time.Duration(float64(sinceLast) * multiplier)
		if buffer < floor {
			buffer = floor
		}
		sinceTime = watermark.RefreshedAt.Add(-buffer)
	}

	base, hasBase, err := gitBoundaryCommit(*repo, sinceTime)
	if err != nil {
		pf(stderr, "error: locate window boundary commit in %s: %v\n", *repo, err)
		return 1
	}
	diffBase := base
	if !hasBase {
		diffBase = docsChurnEmptyTreeObject
	}

	commits, err := gitCommitsInRange(*repo, diffBase, head)
	if err != nil {
		pf(stderr, "error: list commits in %s: %v\n", *repo, err)
		return 1
	}
	changed, err := gitChangedFiles(*repo, diffBase, head)
	if err != nil {
		pf(stderr, "error: list changed files in %s: %v\n", *repo, err)
		return 1
	}

	digest := buildDocsChurnDigest(firstRun, sinceTime, head, base, hasBase, watermark, haveWatermark,
		multiplier, floor, commits, changed, docsRoots)

	if code := writeDocsChurnDigest(digest, stdout, stderr); code != 0 {
		return code
	}

	if advance {
		if err := writeDocsWatermark(wmPath, docsWatermark{
			Schema:      docsWatermarkSchemaVersion,
			Gaggle:      gaggle,
			Workflow:    workflow,
			SHA:         head,
			RefreshedAt: now,
		}); err != nil {
			pf(stderr, "error: advance docs watermark %s: %v\n", wmPath, err)
			return 1
		}
	}
	return 0
}

func buildDocsChurnDigest(
	firstRun bool,
	sinceTime time.Time,
	head, base string,
	hasBase bool,
	watermark docsWatermark,
	haveWatermark bool,
	multiplier float64,
	floor time.Duration,
	commits []churnCommit,
	changed []string,
	docsRoots []string,
) docsChurnDigest {
	if commits == nil {
		commits = []churnCommit{}
	}
	if changed == nil {
		changed = []string{}
	}
	digest := docsChurnDigest{
		Schema:           docsChurnSchemaVersion,
		FirstRun:         firstRun,
		Since:            sinceTime,
		Head:             head,
		BufferMultiplier: multiplier,
		SinceFloor:       floor.String(),
		CommitCount:      len(commits),
		Commits:          commits,
		ChangedFiles:     changed,
		Areas:            groupByArea(changed),
		DocsRoots:        docsRoots,
		DocsRootChanges:  filesUnderRoots(docsRoots, changed),
	}
	if hasBase {
		digest.Base = base
	}
	if haveWatermark {
		wm := watermark
		digest.Watermark = &wm
	}
	switch {
	case len(changed) == 0 && firstRun:
		digest.NoWork = true
		digest.Note = docsChurnFirstRunNote + "; " + docsChurnNoChurnNote
	case len(changed) == 0:
		digest.NoWork = true
		digest.Note = docsChurnNoChurnNote
	case firstRun:
		digest.Note = docsChurnFirstRunNote
	}
	return digest
}

func writeDocsChurnDigest(digest docsChurnDigest, stdout, stderr io.Writer) int {
	out, err := json.MarshalIndent(digest, "", "  ")
	if err != nil {
		pf(stderr, "error: encode churn digest: %v\n", err)
		return 1
	}
	out = append(out, '\n')
	if rf := providerInput(executor.InputResultFile, ""); rf != "" {
		if err := os.WriteFile(rf, out, 0o644); err != nil {
			pf(stderr, "error: write result file %q: %v\n", rf, err)
			return 1
		}
		return 0
	}
	if _, err := stdout.Write(out); err != nil {
		return 2
	}
	return 0
}

// groupByArea buckets changed paths by their top-level path segment (a file at
// the repo root is grouped under "(root)"), giving the docs goober a coarse map
// of where the change landed without re-deriving it from the flat file list.
func groupByArea(changed []string) map[string][]string {
	areas := map[string][]string{}
	for _, f := range changed {
		clean := filepath.ToSlash(filepath.Clean(f))
		area := "(root)"
		if i := strings.IndexByte(clean, '/'); i > 0 {
			area = clean[:i]
		}
		areas[area] = append(areas[area], f)
	}
	for _, files := range areas {
		sort.Strings(files)
	}
	return areas
}

// filesUnderRoots returns the changed files contained within any declared docs
// root, so the digest flags which churn already landed in documentation (vs
// code that may have drifted its docs). Returns nil when no roots are declared.
func filesUnderRoots(roots, changed []string) []string {
	if len(roots) == 0 {
		return nil
	}
	var hits []string
	for _, f := range changed {
		if configboundary.ConfineToAny(roots, []string{f}) == nil {
			hits = append(hits, f)
		}
	}
	sort.Strings(hits)
	return hits
}

func parseDocsRoots(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	var roots []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			roots = append(roots, f)
		}
	}
	return roots
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- watermark persistence -------------------------------------------------

func readDocsWatermark(path string) (docsWatermark, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return docsWatermark{}, false, nil
		}
		return docsWatermark{}, false, err
	}
	var wm docsWatermark
	if err := json.Unmarshal(data, &wm); err != nil {
		return docsWatermark{}, false, fmt.Errorf("parse watermark: %w", err)
	}
	if wm.SHA == "" || wm.RefreshedAt.IsZero() {
		return docsWatermark{}, false, fmt.Errorf("watermark %s is missing sha/refreshedAt", path)
	}
	return wm, true, nil
}

func writeDocsWatermark(path string, wm docsWatermark) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(wm, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// Write-then-rename so a crash mid-write can never leave a truncated
	// watermark that the next run would reject and refuse to advance.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return durability.ReplaceFile(tmp, path)
}

// --- git plumbing ----------------------------------------------------------

func gitRevParse(repo, rev string) (string, error) {
	out, err := gitOutput(repo, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// gitBoundaryCommit returns the most recent commit whose committer date is
// strictly before sinceTime — the stable diff base at the far edge of the
// window. hasBase is false when the window reaches back before the first commit
// (the whole history is in range), in which case the caller diffs against the
// empty tree instead.
func gitBoundaryCommit(repo string, sinceTime time.Time) (string, bool, error) {
	out, err := gitOutput(repo, "rev-list", "-1",
		"--before="+sinceTime.UTC().Format(time.RFC3339), "HEAD")
	if err != nil {
		return "", false, err
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", false, nil
	}
	return sha, true, nil
}

// gitCommitsInRange returns the commits in (base, head], newest first. base is a
// commit SHA or git's empty-tree object (whole-history case); the empty tree is
// not a commit, so that case lists every commit reachable from head.
func gitCommitsInRange(repo, base, head string) ([]churnCommit, error) {
	const sep = "\x1e" // record separator between fields
	format := "%H" + sep + "%s" + sep + "%b"
	rangeArg := base + ".." + head
	if base == docsChurnEmptyTreeObject {
		rangeArg = head
	}
	out, err := gitOutput(repo, "log", "-z", "--no-color", "--format="+format, rangeArg)
	if err != nil {
		return nil, err
	}
	var commits []churnCommit
	for _, rec := range strings.Split(out, "\x00") {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		parts := strings.SplitN(rec, sep, 3)
		if len(parts) < 2 {
			continue
		}
		c := churnCommit{SHA: strings.TrimSpace(parts[0]), Subject: strings.TrimSpace(parts[1])}
		if len(parts) == 3 {
			c.Body = strings.TrimSpace(parts[2])
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// gitChangedFiles returns the repo-relative paths changed between base and head.
// --no-renames so a file moved out of a docs root surfaces as its new path
// rather than being hidden by rename detection, matching configboundary's diff.
func gitChangedFiles(repo, base, head string) ([]string, error) {
	out, err := gitOutput(repo, "diff", "--no-renames", "--name-only", base, head)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	sort.Strings(files)
	return files, nil
}

func gitOutput(repo string, args ...string) (string, error) {
	full := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
