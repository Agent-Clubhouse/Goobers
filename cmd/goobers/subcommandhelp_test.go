package main

import (
	"os"
	"strings"
	"testing"
)

// TestSubcommandHelpWritesToStdoutAndExitsZero is #4832's regression guard.
// `--help`/`-h` on any subcommand was routed through the handler's own
// flag.FlagSet.Parse, which cannot distinguish flag.ErrHelp from a genuine
// usage error: both hit the uniform `if err := fs.Parse(args); err != nil {
// return 2 }`, so a help request wrote its (correct) text to stderr and
// exited 2 instead of writing to stdout and exiting 0 — breaking any script
// or CI step that shells out to `<command> --help` and checks the exit code
// or reads stdout. docs/guides/ado-authentication.md:111 instructs readers
// to run exactly `goobers report-pr-status --help`; this test iterates every
// registered command (cmd/goobers/runtime_capabilities.go's cliCommands,
// via the same helpGoldenCommands walk TestCLIHelpGolden uses) so that
// command, and every other one present and future, is covered.
func TestSubcommandHelpWritesToStdoutAndExitsZero(t *testing.T) {
	unsetRunContext(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	commands := helpGoldenCommands(cliCommands, nil)
	if len(commands) == 0 {
		t.Fatal("helpGoldenCommands enumerated no commands")
	}
	for _, path := range commands {
		name := strings.Join(path, " ")
		for _, flag := range []string{"--help", "-h"} {
			t.Run(name+"/"+flag, func(t *testing.T) {
				args := append(append([]string{}, path...), flag)
				code, stdout, stderr := runArgs(t, args...)
				if code != 0 {
					t.Errorf("`goobers %s %s` exit code = %d, want 0 (stderr: %q)", name, flag, code, stderr)
				}
				if strings.TrimSpace(stdout) == "" {
					t.Errorf("`goobers %s %s` wrote nothing to stdout (stderr: %q)", name, flag, stderr)
				}
			})
		}
	}
}
