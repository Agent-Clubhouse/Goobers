// Command goobers-cloud hosts the cloud control-plane roles.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/version"
)

type command struct {
	name     string
	short    string
	long     string
	examples []string
	run      func([]string, io.Writer, io.Writer) int
	role     bool
	hidden   bool
}

var commands []command

func init() {
	commands = []command{
		{name: "api", short: "Serve the cloud control-plane API.", long: apiHelp, run: runAPIRole, role: true},
		{name: "controller", short: "Run cloud control-plane reconciliation.", long: controllerHelp, run: runControllerRole, role: true},
		{name: "worker", short: "Run a Temporal engine worker.", long: workerHelp, examples: []string{"goobers-cloud worker --task-queue goobers"}, run: runWorkerRole, role: true},
		{name: "migrate", short: "Apply cloud data-store migrations.", long: migrateHelp, run: runMigrateRole, role: true},
		{name: "bootstrap", short: "Prepare a new cloud install for cutover.", long: bootstrapHelp, run: runBootstrapRole},
		{name: "cutover", short: "Check cutover readiness of a cloud install.", long: cutoverHelp, run: runCutoverRole},
		{name: "completion", short: "Generate a shell completion script.", long: completionHelp, run: runCompletion},
		{name: "help", short: "Show command help.", long: helpHelp, run: runHelp},
		{name: "__generate-docs", run: runGenerateDocs, hidden: true},
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func write(w io.Writer, text string) {
	_, _ = io.WriteString(w, text)
}

func writef(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func writeln(w io.Writer, text string) {
	_, _ = fmt.Fprintln(w, text)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeUsage(stderr)
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" {
		writeUsage(stdout)
		return 0
	}
	for _, candidate := range commands {
		if candidate.name == args[0] {
			return candidate.run(args[1:], stdout, stderr)
		}
	}
	writef(stderr, "goobers-cloud: unknown command %q\n\n", args[0])
	writeUsage(stderr)
	return 2
}

func writeUsage(w io.Writer) {
	writef(w, "goobers-cloud %s\n\nUsage:\n  goobers-cloud <COMMAND> [OPTIONS]\n\nRoles:\n", version.Version)
	visible := make([]command, 0, len(commands))
	for _, candidate := range commands {
		if candidate.role {
			visible = append(visible, candidate)
		}
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].name < visible[j].name })
	for _, candidate := range visible {
		writef(w, "  %-12s %s\n", candidate.name, candidate.short)
	}
	writeln(w, "\nCommands:")
	for _, candidate := range commands {
		if !candidate.hidden && !candidate.role {
			writef(w, "  %-12s %s\n", candidate.name, candidate.short)
		}
	}
	writeln(w, "\nRun `goobers-cloud help COMMAND` for command details.")
}

const helpHelp = `Usage: goobers-cloud help [COMMAND]

Show top-level help or detailed help for one role.
`

func runHelp(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeUsage(stdout)
		return 0
	}
	if len(args) != 1 {
		write(stderr, helpHelp)
		return 2
	}
	for _, candidate := range commands {
		if !candidate.hidden && candidate.name == args[0] {
			write(stdout, candidate.long)
			return 0
		}
	}
	writef(stderr, "goobers-cloud help: unknown command %q\n", strings.Join(args, " "))
	return 2
}
