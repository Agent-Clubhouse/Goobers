package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/docchurn"
	"github.com/goobers/goobers/internal/executor"
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
	docsChurnFormat        = "churn-digest"
	docsChurnDefaultFloor  = 168 * time.Hour
	docsChurnDefaultBuffer = 3.0
)

type docsChurnDigest = docchurn.Digest

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
	fs := newCLIFlagSet("docs-churn", flag.ContinueOnError)
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
		d, err := parseDurationInput(
			v,
			func(value time.Duration) bool { return value > 0 },
			func(raw string, _ error) string {
				return fmt.Sprintf("input sinceFloor must be a positive duration, got %q", raw)
			},
		)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		floor = d
	}
	multiplier := *bufferMultiplier
	if v := providerInput("bufferMultiplier", ""); v != "" {
		m, err := parseFloatInput(
			v,
			func(value float64) bool { return !(value < 1) },
			func(raw string, _ error) string {
				return fmt.Sprintf("input bufferMultiplier must be a number >= 1, got %q", raw)
			},
		)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		multiplier = m
	}

	workflow := firstNonEmpty(*workflowFlag, os.Getenv(executor.WorkflowEnvVar))
	if workflow == "" {
		pf(stderr, "error: --workflow or $GOOBERS_WORKFLOW is required (the docs watermark is per-workflow)\n")
		return 2
	}
	gaggle := firstNonEmpty(*gaggleFlag, providerGaggle())
	docsRoots := parseDocsRoots(providerInput("docsRoots", ""))
	advance := providerInput("advanceWatermark", "true") == "true"

	root := providerStageRoot(pathArg)
	wmPath := layoutFor(root).DocsWatermarkPath(gaggle, workflow)
	err := docchurn.Run(docchurn.Options{
		Repo:             *repo,
		WatermarkPath:    wmPath,
		Gaggle:           gaggle,
		Workflow:         workflow,
		ResultFile:       providerInput(executor.InputResultFile, ""),
		SinceFloor:       floor,
		BufferMultiplier: multiplier,
		DocsRoots:        docsRoots,
		AdvanceWatermark: advance,
		Clock:            time.Now,
		Git:              gitOutput,
		Stdout:           stdout,
	})
	if err != nil {
		var outputErr *docchurn.StdoutError
		if errors.As(err, &outputErr) {
			return 2
		}
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
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

// --- git plumbing ----------------------------------------------------------

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
