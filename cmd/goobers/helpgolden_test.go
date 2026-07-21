package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestCLIHelpGolden locks the full rendered CLI help surface — the top-level
// usage() plus every command's own `-h` help — into a golden file. It is the
// regression guard for #1095 (CLI-1): the help lift onto the command registry
// must not change a single byte of what a user sees. Regenerate intentionally
// with UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestCLIHelpGolden.
func TestCLIHelpGolden(t *testing.T) {
	unsetRunContext(t)
	// Resolve the golden path against the package directory before we chdir
	// away from it below.
	path, err := filepath.Abs(filepath.Join("testdata", "help.golden"))
	if err != nil {
		t.Fatal(err)
	}
	// Run from a directory with no instance so no command's -h path can read
	// ambient config; help must render before any such side effect anyway.
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

	var b strings.Builder
	capture := func(title string, args ...string) {
		_, stdout, stderr := runArgs(t, args...)
		b.WriteString("### " + title + "\n")
		b.WriteString(stdout)
		b.WriteString(stderr)
		if !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	capture("(bare)")
	capture("help", "help")
	// Sort by invocation path so the golden layout is stable regardless of the
	// registry's declaration order — the file locks each command's help text,
	// not the registry ordering.
	commands := helpGoldenCommands(cliCommands, nil)
	sort.Slice(commands, func(i, j int) bool {
		return strings.Join(commands[i], " ") < strings.Join(commands[j], " ")
	})
	for _, cmd := range commands {
		capture(strings.Join(cmd, " "), append(append([]string{}, cmd...), "-h")...)
	}

	got := b.String()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("CLI help surface differs from %s; if this change is intentional, regenerate with UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestCLIHelpGolden", path)
	}
}

// helpGoldenCommands returns the invocation path of every user-facing command
// and subcommand, skipping internal/hidden entrypoints that carry no help.
func helpGoldenCommands(commands []cliCommand, prefix []string) [][]string {
	var out [][]string
	for _, command := range commands {
		if len(command.names) == 0 {
			continue
		}
		name := command.names[0]
		// Skip internal entrypoints (no help) and the -h/--version flag
		// aliases: help duplicates the bare-usage capture and --version emits a
		// build-dependent string covered by TestRunVersion.
		if strings.HasPrefix(name, "__") || strings.HasPrefix(name, "-") || name == detachedRunWorkerCommand {
			continue
		}
		path := append(append([]string{}, prefix...), name)
		out = append(out, path)
		out = append(out, helpGoldenCommands(command.subcommands, path)...)
	}
	return out
}
