// Command goobers-cloud hosts the cloud control-plane roles.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/cliregistry"
	"github.com/goobers/goobers/internal/cloud/operatorcli"
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
		{name: "migrate", short: "Apply cloud data-store migrations.", long: migrateHelp, run: runMigrateRole, role: true},
		{name: "worker", short: "Run a Temporal engine worker.", long: workerHelp, examples: []string{"goobers-cloud worker --task-queue goobers"}, run: runWorkerRole, role: true},
		{name: "completion", short: "Generate a shell completion script.", long: completionHelp, run: runCompletion},
		{name: "help", short: "Show command help.", long: helpHelp, run: runHelp},
		{name: "__generate-docs", run: runGenerateDocs, hidden: true},
	}
	for _, operator := range operatorcli.Commands(map[string]operatorcli.Handler{
		"migrate status": runMigrateStatus,
		"lease status":   runLeaseStatus,
	}) {
		commands = append(commands, command{name: operator.Name, short: operator.Short, long: operator.Long, run: operator.Run})
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

func pf(w io.Writer, format string, args ...any) { writef(w, format, args...) }

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
	if candidate, consumed, ok := findCommand(args); ok {
		return candidate.run(args[consumed:], stdout, stderr)
	}
	writef(stderr, "goobers-cloud: unknown command %q\n\n", args[0])
	writeUsage(stderr)
	return 2
}

func writeUsage(w io.Writer) {
	writef(w, "goobers-cloud %s\n\nUsage:\n  goobers-cloud <COMMAND> [OPTIONS]\n\nRoles:\n", version.Version)
	for _, candidate := range commands {
		if candidate.role {
			writef(w, "  %-18s %s\n", candidate.name, candidate.short)
		}
	}
	writeln(w, "\nCommands:")
	for _, candidate := range commands {
		if !candidate.hidden && !candidate.role {
			writef(w, "  %-18s %s\n", candidate.name, candidate.short)
		}
	}
	writeln(w, "\nRun `goobers-cloud help COMMAND` for command details.")
}

const helpHelp = `Usage: goobers-cloud help [COMMAND]

Show top-level help or detailed help for one command path.
`

func runHelp(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeUsage(stdout)
		return 0
	}
	name := strings.Join(args, " ")
	for _, candidate := range commands {
		if !candidate.hidden && candidate.name == name {
			write(stdout, candidate.long)
			return 0
		}
	}
	writef(stderr, "goobers-cloud help: unknown command %q\n", strings.Join(args, " "))
	return 2
}

func findCommand(args []string) (command, int, bool) {
	names := make([]string, len(commands))
	for i := range commands {
		names[i] = commands[i].name
	}
	index, consumed, ok := cliregistry.Match(names, args)
	if !ok {
		return command{}, 0, false
	}
	return commands[index], consumed, true
}
