// Command goobers is the tier 1-2 instance CLI (INST-012, DEP-021/022):
// `goobers init` scaffolds an instance root, `goobers validate` checks it,
// `goobers up`/`run` operate it, and `goobers status`/`trace` inspect it
// (ARCHITECTURE.md §6).
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	_ "time/tzdata"

	"github.com/goobers/goobers/internal/version"
	"golang.org/x/term"
)

const (
	ansiReset      = "\x1b[0m"
	ansiBoldCyan   = "\x1b[1;36m"
	ansiBoldGreen  = "\x1b[1;32m"
	ansiBoldYellow = "\x1b[1;33m"
	ansiMagenta    = "\x1b[35m"
	ansiCyan       = "\x1b[36m"
)

// runProcessExits is true only for the real CLI entrypoint. In-process callers
// keep standalone asynchronous runs alive in their host process instead.
var runProcessExits bool

func main() {
	runProcessExits = true
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// pf/pln are thin print helpers that discard the write error — these are
// terminal CLI writes to stdout/stderr where a failed write is not
// actionable.
func pf(w io.Writer, format string, a ...interface{}) { _, _ = fmt.Fprintf(w, format, a...) }
func pln(w io.Writer, s string)                       { _, _ = fmt.Fprintln(w, s) }

func cliColorEnabled(w io.Writer) bool {
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled {
		return false
	}
	file, ok := w.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

func cliStyled(w io.Writer, style, text string) string {
	if !cliColorEnabled(w) {
		return text
	}
	return style + text + ansiReset
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	if command, ok := findCLICommand(args[0]); ok {
		return command.dispatch(args[1:], stdout, stderr)
	}
	pf(stderr, "goobers: unknown command %q\n\n", args[0])
	usage(stderr)
	return 2
}

// usage renders the core operator surface. Advanced operator and runner-invoked
// stage commands remain available through explicit help views.
func usage(w io.Writer) {
	writeUsageHeader(w)
	writeSynopses(w, cliCommands, cliTierCore)
	writeUsageFooter(w)
}

func writeUsageHeader(w io.Writer) {
	separator := "================================================================"
	title := fmt.Sprintf("goobers %s", version.Version)
	pf(
		w,
		"%s\n%s\n%s\n\n%s\n%s\n\n%s\n",
		cliStyled(w, ansiBoldCyan, separator),
		cliStyled(w, ansiBoldCyan, title),
		cliStyled(w, ansiBoldCyan, separator),
		cliStyled(w, ansiBoldGreen, "Usage:"),
		colorizeCLISyntax(w, "  goobers <COMMAND> [OPTIONS]", true),
		cliStyled(w, ansiBoldYellow, "Commands:"),
	)
}

func usageAll(w io.Writer) {
	pf(w, usageAllHeader)
	writeSynopsisGroup(w, "Core commands:", cliTierCore)
	writeSynopsisGroup(w, "Advanced operator commands:", cliTierAdvanced)
	writeSynopsisGroup(w, "Workflow-stage and connector commands:", cliTierStage)
	pf(w, usageAllFooter)
}

func usageStages(w io.Writer) {
	pf(w, usageStagesHeader)
	writeSynopses(w, cliCommands, cliTierStage)
	pf(w, usageStagesFooter)
}

func writeSynopsisGroup(w io.Writer, heading string, tier cliCommandTier) {
	pln(w, cliStyled(w, ansiBoldYellow, heading))
	writeSynopses(w, cliCommands, tier)
	pln(w, "")
}

// writeSynopses walks the registry in declaration order and emits entries from
// one tier. Commands without a synopsis are skipped.
func writeSynopses(w io.Writer, commands []cliCommand, tier cliCommandTier) {
	entries := collectCommandIndexEntries(commands, tier, nil)
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].command) < strings.ToLower(entries[j].command)
	})
	for _, entry := range entries {
		writeCommandIndexEntry(w, entry.command, entry.description)
	}
}

type commandIndexEntry struct {
	command     string
	description string
}

func collectCommandIndexEntries(commands []cliCommand, tier cliCommandTier, prefix []string) []commandIndexEntry {
	var entries []commandIndexEntry
	for _, command := range commands {
		path := prefix
		if name := docDisplayName(command); name != "" {
			path = append(append([]string{}, prefix...), name)
		}
		if command.synopsis != "" && command.tier == tier {
			displayCommand := strings.Join(path, " ")
			entries = append(entries, commandIndexEntry{
				command:     displayCommand,
				description: commandIndexDescription(displayCommand, command.short),
			})
		}
		entries = append(entries, collectCommandIndexEntries(command.subcommands, tier, path)...)
	}
	return entries
}

func writeCommandIndexEntry(w io.Writer, command, description string) {
	const descriptionColumn = 20
	commandPrefix := "  " + command
	if strings.TrimSpace(description) == "" {
		pln(w, "  "+cliStyled(w, ansiMagenta, command))
		return
	}

	padding := 1
	if len(commandPrefix) < descriptionColumn {
		padding = descriptionColumn - len(commandPrefix)
	}
	pf(
		w,
		"  %s%s%s\n",
		cliStyled(w, ansiMagenta, command),
		strings.Repeat(" ", padding),
		description,
	)
}

var coreCommandIndexDescriptions = map[string]string{
	"completion":      "Generate shell completion scripts.",
	"connect":         "Connect an instance to a GitHub repository.",
	"dashboard":       "Open the local operations portal.",
	"down":            "Gracefully stop a running daemon.",
	"escalations":     "List escalated runs.",
	"examples":        "Browse embedded workflow examples.",
	"getting-started": "Open the guided Getting Started walkthrough.",
	"init":            "Create an instance or configuration source.",
	"run":             "Start a workflow run.",
	"scaffold":        "Create a goober, workflow, or gaggle.",
	"service":         "Install and manage the supervised daemon.",
	"signal":          "Fire an external workflow signal.",
	"stats":           "Show instance lifetime statistics.",
	"status":          "Show instance, daemon, run, or agent status.",
	"trace":           "Inspect a run's journal and transcripts.",
	"up":              "Start the local daemon.",
	"validate":        "Validate an instance or configuration source.",
	"version":         "Show build version information.",
	"workflow show":   "Show a workflow as a text DAG.",
}

func commandIndexDescription(command, fallback string) string {
	if description, ok := coreCommandIndexDescriptions[command]; ok {
		return description
	}
	return fallback
}

func colorizeCLISyntax(w io.Writer, line string, hasCommand bool) string {
	if !cliColorEnabled(w) {
		return line
	}
	indentLength := len(line) - len(strings.TrimLeft(line, " "))
	indent := line[:indentLength]
	content := line[indentLength:]
	if !hasCommand {
		return indent + ansiCyan + content + ansiReset
	}
	command, parameters, found := strings.Cut(content, " ")
	if !found {
		return indent + ansiMagenta + command + ansiReset
	}
	return indent + ansiMagenta + command + ansiReset + " " +
		ansiCyan + parameters + ansiReset
}

func writeUsageFooter(w io.Writer) {
	pln(w, "")
	pln(w, cliStyled(w, ansiBoldYellow, "Defaults:"))
	pln(w, "  Path arguments use the current directory when omitted.")

	pln(w, "")
	pln(w, cliStyled(w, ansiBoldYellow, "Exit codes:"))
	pln(w, "  0  Success or successful submission")
	pln(w, "  1  Validation/business failure, failed run, or aborted run")
	pln(w, "  2  Usage or I/O error")
	pln(w, "  3  Escalated run or signal")
	pln(w, "  Submission-only modes return 0 before the final outcome is known.")

	pln(w, "")
	pln(w, cliStyled(w, ansiBoldYellow, "More help:"))
	pf(w, "  Advanced commands  %s\n", cliStyled(w, ansiCyan, "goobers help all"))
	pf(w, "  Workflow internals %s\n", cliStyled(w, ansiCyan, "goobers help stages"))
	pf(w, "  Command details    %s\n", cliStyled(w, ansiCyan, "goobers help <command>"))
	pln(w, "  Quickstart         docs/guides/quickstart.md")
	pf(w, "  DSL                %s, %s\n", cliStyled(w, ansiCyan, "goobers schema"), cliStyled(w, ansiCyan, "goobers examples"))
	pf(w, "  Troubleshooting    %s, %s, %s\n", cliStyled(w, ansiCyan, "goobers status"), cliStyled(w, ansiCyan, "goobers trace"), cliStyled(w, ansiCyan, "goobers escalations"))
}

const usageAllHeader = `goobers — complete command reference

Usage:

`

const usageAllFooter = `path defaults to the current directory.
`

const usageStagesHeader = `goobers — workflow-stage and connector commands

These commands are invoked by the runner, not typically by hand. They read
their run context from GOOBERS_* environment variables and retain their
existing standalone invocation behavior.

Usage:
`

const usageStagesFooter = `
See ` + "`goobers --help`" + ` for core operator commands.
`
