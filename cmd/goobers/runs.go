package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func runRuns(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		pf(w, "Usage: goobers runs <command> [flags] [path]\n\n"+
			"Commands:\n"+
			"  list    list runs, most-recent first\n")
	}
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runRunsList(args[1:], stdout, stderr)
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

// runSummary is the flat, journal-derived row the run-listing commands print.
type runSummary struct {
	RunID     string
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
			continue
		}
		id, err := reader.Identity()
		if err != nil {
			continue
		}
		out = append(out, runSummary{
			RunID:     id.RunID,
			Workflow:  id.Workflow,
			Gaggle:    id.Gaggle,
			Phase:     runPhase(reader),
			StartedAt: id.StartedAt,
		})
	}
	return out, nil
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
