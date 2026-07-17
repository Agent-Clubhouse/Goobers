package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func runRuns(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		pf(w, "Usage: goobers runs <command> [flags] [path]\n\n"+
			"Commands:\n"+
			"  list    list runs, most-recent first\n"+
			"  du      report per-run journal and artifact bytes, largest first\n")
	}
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runRunsList(args[1:], stdout, stderr)
	case "du":
		return runRunsDU(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		pf(stderr, "goobers runs: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func runRunsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runs list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit run summaries as JSON")
	limit := fs.Int("limit", 0, "maximum number of runs to show (default: all)")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers runs list [--json] [--limit=N] [path]\n\n"+
			"List runs under an instance's runs/ directory, most-recent first\n"+
			"(default path \".\"). Exit codes: 0 = OK, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *limit < 0 || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}

	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}
	runs, err := listRuns(l.RunsDir())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].RunID < runs[j].RunID
		}
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
	if *limit > 0 && len(runs) > *limit {
		runs = runs[:*limit]
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(statusJSONSummaries(runs)); err != nil {
			pf(stderr, "error: encode runs list: %v\n", err)
			return 2
		}
		return 0
	}
	if len(runs) == 0 {
		pln(stdout, "no runs found")
		return 0
	}

	pf(stdout, "%-34s  %-24s  %-10s  %-10s  %s\n", "RUN ID", "WORKFLOW", "GAGGLE", "PHASE", "STARTED")
	for _, r := range runs {
		pf(stdout, "%-34s  %-24s  %-10s  %-10s  %s\n",
			r.RunID, r.Workflow, r.Gaggle, r.Phase, r.StartedAt.Format(time.RFC3339))
	}
	return 0
}

type runDiskUsage struct {
	RunID             string `json:"runId"`
	JournalStateBytes int64  `json:"journalStateBytes"`
	ArtifactBytes     int64  `json:"artifactBytes"`
	TotalBytes        int64  `json:"totalBytes"`
}

type runsDiskUsage struct {
	Runs              []runDiskUsage `json:"runs"`
	JournalStateBytes int64          `json:"journalStateBytes"`
	ArtifactBytes     int64          `json:"artifactBytes"`
	TotalBytes        int64          `json:"totalBytes"`
}

func runRunsDU(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runs du", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit disk usage as JSON")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers runs du [--json] [path]\n\n"+
			"Report file bytes used by each run, split between journal/state data\n"+
			"and artifacts, largest first (default path \".\").\n"+
			"Exit codes: 0 = OK, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}

	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}
	runs, err := listRunsStrict(l.RunsDir())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	usage, err := measureRunsDiskUsage(l.RunsDir(), runs)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(usage); err != nil {
			pf(stderr, "error: encode runs disk usage: %v\n", err)
			return 2
		}
		return 0
	}

	pf(stdout, "%-34s  %18s  %14s  %11s\n", "RUN ID", "JOURNAL/STATE BYTES", "ARTIFACT BYTES", "TOTAL BYTES")
	for _, run := range usage.Runs {
		pf(stdout, "%-34s  %18d  %14d  %11d\n",
			run.RunID, run.JournalStateBytes, run.ArtifactBytes, run.TotalBytes)
	}
	pf(stdout, "%-34s  %18d  %14d  %11d\n",
		"runs/ total", usage.JournalStateBytes, usage.ArtifactBytes, usage.TotalBytes)
	return 0
}

func measureRunsDiskUsage(runsDir string, runs []runSummary) (runsDiskUsage, error) {
	usage := runsDiskUsage{Runs: make([]runDiskUsage, 0, len(runs))}
	for _, run := range runs {
		runUsage, err := measureRunDiskUsage(filepath.Join(runsDir, run.DirName), run.RunID)
		if err != nil {
			return runsDiskUsage{}, err
		}
		usage.Runs = append(usage.Runs, runUsage)
		usage.JournalStateBytes += runUsage.JournalStateBytes
		usage.ArtifactBytes += runUsage.ArtifactBytes
		usage.TotalBytes += runUsage.TotalBytes
	}
	sort.Slice(usage.Runs, func(i, j int) bool {
		if usage.Runs[i].TotalBytes == usage.Runs[j].TotalBytes {
			return usage.Runs[i].RunID < usage.Runs[j].RunID
		}
		return usage.Runs[i].TotalBytes > usage.Runs[j].TotalBytes
	})
	return usage, nil
}

func measureRunDiskUsage(runDir, runID string) (runDiskUsage, error) {
	usage := runDiskUsage{RunID: runID}
	artifactPrefix := filepath.Join(runDir, "artifacts") + string(os.PathSeparator)
	err := filepath.WalkDir(runDir, func(path string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if strings.HasPrefix(path, artifactPrefix) {
			usage.ArtifactBytes += info.Size()
		} else {
			usage.JournalStateBytes += info.Size()
		}
		usage.TotalBytes += info.Size()
		return nil
	})
	if err == nil {
		return usage, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return runDiskUsage{}, fmt.Errorf(
			"run %q changed or was removed while calculating disk usage; retry `goobers runs du`: %w",
			runID, err,
		)
	}
	return runDiskUsage{}, fmt.Errorf("calculate disk usage for run %q: %w", runID, err)
}

// runSummary is the flat, journal-derived row the run-listing commands print.
type runSummary struct {
	RunID     string
	DirName   string
	Workflow  string
	Gaggle    string
	Phase     journal.RunPhase
	StartedAt time.Time
}

// listRuns scans an instance's runs/ directory for run subdirectories and
// summarizes each via the journal reader. A missing runs/ directory yields an
// empty list, not an error (a freshly-init'd instance has none yet); an entry
// that isn't a readable run directory is skipped rather than failing the whole
// listing — run listings are best-effort over what's actually there.
func listRuns(runsDir string) ([]runSummary, error) {
	return listRunsWithPolicy(runsDir, false)
}

func listRunsStrict(runsDir string) ([]runSummary, error) {
	return listRunsWithPolicy(runsDir, true)
}

func listRunsWithPolicy(runsDir string, strict bool) ([]runSummary, error) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs directory: %w", err)
	}
	var out []runSummary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, entry.Name())
		reader, err := journal.OpenRead(dir)
		if err != nil {
			if strict {
				return nil, runEntryError(entry.Name(), err)
			}
			continue
		}
		id, err := reader.Identity()
		if err != nil {
			if strict {
				return nil, runEntryError(entry.Name(), err)
			}
			continue
		}
		out = append(out, runSummary{
			RunID:     id.RunID,
			DirName:   entry.Name(),
			Workflow:  id.Workflow,
			Gaggle:    id.Gaggle,
			Phase:     runPhase(reader),
			StartedAt: id.StartedAt,
		})
	}
	return out, nil
}

func runEntryError(name string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"run entry %q disappeared or is incomplete; confirm it still exists, then retry `goobers runs du`: %w",
			name, err,
		)
	}
	return fmt.Errorf("inspect run entry %q: %w", name, err)
}

// runPhase prefers the state.json checkpoint (the fast path); if it's missing —
// e.g. a run whose first checkpoint hasn't landed yet — it falls back to the
// terminal run.finished event's Status, the same source of truth
// journal.Recover reconstructs the phase from. A run with neither is still
// running.
func runPhase(reader *journal.Reader) journal.RunPhase {
	if st, err := reader.State(); err == nil {
		return st.Phase
	}
	events, err := reader.Events()
	if err != nil {
		return journal.PhaseRunning
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != journal.EventRunFinished {
			continue
		}
		switch phase := journal.RunPhase(events[i].Status); phase {
		case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
			return phase
		}
	}
	return journal.PhaseRunning
}
