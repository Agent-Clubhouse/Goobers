package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

const recentCompletionRunLimit = 100

func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		completionUsage(stdout)
		return 0
	}
	completionUsage(stderr)
	return 2
}

func runCompletionScript(script string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		completionUsage(stderr)
		return 2
	}
	pf(stdout, "%s", script)
	return 0
}

const completionHelp = "Usage: goobers completion <bash|zsh|fish>\n\n" +
	"Generate a shell completion script. Source the output in the target shell.\n"

func completionUsage(w io.Writer) {
	pf(w, "%s", completionHelp)
}

// runCompletionCandidates is intentionally hidden from help. Generated scripts
// call it on every dynamic completion request, so all discovery failures are
// silent and leave the shell prompt usable.
func runCompletionCandidates(args []string, stdout io.Writer) int {
	if len(args) != 1 {
		return 0
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 0
	}
	for _, candidate := range completionCandidates(args[0], cwd) {
		pln(stdout, candidate)
	}
	return 0
}

func completionCandidates(kind, start string) []string {
	root, ok := completionInstanceRoot(start)
	if !ok {
		return nil
	}
	layout := instance.NewLayout(root)

	switch kind {
	case "workflows":
		set, report, err := instance.LoadConfigDir(layout.ConfigDir())
		if err != nil || report == nil {
			return nil
		}
		names := make([]string, 0, len(set.Workflows))
		seen := make(map[string]struct{}, len(set.Workflows))
		for _, workflow := range set.Workflows {
			if workflow.Name == "" {
				continue
			}
			if _, exists := seen[workflow.Name]; exists {
				continue
			}
			seen[workflow.Name] = struct{}{}
			names = append(names, workflow.Name)
		}
		sort.Strings(names)
		return names
	case "runs":
		runs, err := listLayoutRuns(layout, false)
		if err != nil {
			return nil
		}
		return recentCompletionRunIDs(runs)
	case "escalations":
		runs, err := listLayoutRuns(layout, false)
		if err != nil {
			return nil
		}
		escalated := make([]runSummary, 0, len(runs))
		for _, run := range runs {
			if run.Phase == journal.PhaseEscalated {
				escalated = append(escalated, run)
			}
		}
		return recentCompletionRunIDs(escalated)
	default:
		return nil
	}
}

func completionInstanceRoot(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		info, err := os.Stat(filepath.Join(dir, instance.ConfigFileName))
		if err == nil && !info.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func recentCompletionRunIDs(runs []runSummary) []string {
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].RunID < runs[j].RunID
		}
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
	if len(runs) > recentCompletionRunLimit {
		runs = runs[:recentCompletionRunLimit]
	}
	ids := make([]string, len(runs))
	for i := range runs {
		ids[i] = runs[i].RunID
	}
	return ids
}

// The shell completion scripts are rendered from the cliCommand registry (via
// buildCompletionModel) rather than hand-written, so the completable command
// surface cannot drift from the CLI's actual commands (#1097). Rendering is
// lazy and cached: the registry is populated in init(), so it is not yet
// available when package-level vars initialize — these accessors build the
// scripts on first use, long after init() has run.
var (
	completionScriptsOnce sync.Once
	bashCompletionScript  string
	zshCompletionScript   string
	fishCompletionScript  string
)

func renderCompletionScripts() {
	m := buildCompletionModel()
	bashCompletionScript = renderBashCompletion(m)
	zshCompletionScript = renderZshCompletion(m)
	fishCompletionScript = renderFishCompletion(m)
}

// bashCompletion returns the bash completion script.
func bashCompletion() string {
	completionScriptsOnce.Do(renderCompletionScripts)
	return bashCompletionScript
}

// zshCompletion returns the zsh completion script.
func zshCompletion() string {
	completionScriptsOnce.Do(renderCompletionScripts)
	return zshCompletionScript
}

// fishCompletion returns the fish completion script.
func fishCompletion() string {
	completionScriptsOnce.Do(renderCompletionScripts)
	return fishCompletionScript
}
