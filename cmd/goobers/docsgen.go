package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/clidocs"
)

// CLI reference generation (#1096, CLI-2). A build-time generator walks the
// command registry (the #1095 single source of truth) and emits a roff man page
// per command plus a Markdown reference under docs/cli/. The output is committed
// and a test (docsgen_test.go) regenerates and diffs it, so the docs can never
// drift from the shipped binary — the same discipline as the JSON-schema parity
// checks. There is deliberately no runtime `goobers docs` command: the design's
// requirement (OQ-1) is the build-time generator, and `make docs` regenerates.

// docsRootShort/docsRootLong describe the binary itself for the top-level
// goobers.1 page and the reference intro. docsRootLong reuses the same footer
// prose the top-level usage() prints, so the two never disagree.
const docsRootShort = "tier 1-2 local instance CLI"

func docsRootLong() string { return strings.TrimSpace(usageFooter) }

// renderCLIDocs produces the full set of generated files, keyed by their path
// relative to the docs/ directory. It is pure — it reads only the command
// registry — so the drift test and the writer share one source.
func renderCLIDocs() map[string]string {
	root := clidocs.Command{Short: docsRootShort, Long: docsRootLong()}
	cmds := collectDocCommands(cliCommands, nil)
	clidocs.Sort(cmds)

	files := map[string]string{
		"cli/README.md":           clidocs.Reference(root, cmds),
		"completion/goobers.bash": bashCompletion(),
		"completion/goobers.fish": fishCompletion(),
		"completion/_goobers":     zshCompletion(),
		"man/goobers.1":           clidocs.ManIndex(root, cmds),
	}
	for _, c := range cmds {
		files["man/"+c.ManFile()] = clidocs.ManPage(c)
	}
	return files
}

// collectDocCommands flattens the registry into the documentable command set.
// A hidden entrypoint (a `__`-prefixed name or the detached run worker) is
// skipped along with its subtree; a command with no short description (a flag
// alias like help, or a bare group) is not itself emitted, but its subcommands
// still are, under the correct invocation path.
func collectDocCommands(commands []cliCommand, prefix []string) []clidocs.Command {
	var out []clidocs.Command
	for _, c := range commands {
		display := docDisplayName(c)
		if display == "" || strings.HasPrefix(display, "__") || display == detachedRunWorkerCommand {
			continue
		}
		path := append(append([]string{}, prefix...), display)
		if c.short != "" {
			out = append(out, clidocs.Command{
				Path:     path,
				Short:    c.short,
				Long:     c.long,
				Examples: c.examples,
			})
		}
		out = append(out, collectDocCommands(c.subcommands, path)...)
	}
	return out
}

// docDisplayName is the command's canonical, user-facing name: the first
// registered name that is not a flag alias (e.g. version registers
// "--version","version" — its display name is "version"). Returns "" when every
// name is a flag alias.
func docDisplayName(c cliCommand) string {
	for _, name := range c.names {
		if !strings.HasPrefix(name, "-") {
			return name
		}
	}
	return ""
}

// writeCLIDocs regenerates the committed docs under docsDir, first removing any
// stale generated man page, reference, or completion script. Used by `make
// docs` via the drift test's update mode and by the release packager through the
// hidden generator command.
func writeCLIDocs(docsDir string) error {
	files := renderCLIDocs()
	wanted := make(map[string]bool, len(files))
	for rel := range files {
		wanted[filepath.FromSlash(rel)] = true
	}

	// Prune stale generated files.
	for _, sub := range []string{"man", "cli", "completion"} {
		dir := filepath.Join(docsDir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			rel := filepath.Join(sub, e.Name())
			if isGeneratedDocFile(rel) && !wanted[rel] {
				if err := os.Remove(filepath.Join(docsDir, rel)); err != nil {
					return err
				}
			}
		}
	}

	for rel, content := range files {
		path := filepath.Join(docsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// isGeneratedDocFile reports whether a docs-relative path is one this generator
// owns, so pruning never touches a hand-authored doc.
func isGeneratedDocFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	switch {
	case strings.HasPrefix(rel, "man/") && strings.HasSuffix(rel, ".1"):
		return true
	case rel == "cli/README.md":
		return true
	case rel == "completion/goobers.bash" ||
		rel == "completion/goobers.fish" ||
		rel == "completion/_goobers":
		return true
	default:
		return false
	}
}

func runGenerateDocs(args []string, _ io.Writer, stderr io.Writer) int {
	if len(args) != 1 {
		pln(stderr, "usage: goobers __generate-docs <docs-directory>")
		return 2
	}
	if err := writeCLIDocs(args[0]); err != nil {
		pf(stderr, "generate docs: %v\n", err)
		return 1
	}
	return 0
}

// docSlugsUnique reports the first duplicate man-page slug across cmds, if any.
// Two distinct commands whose hyphen-joined paths collide (e.g. a "backlog-query"
// command and a hypothetical "backlog query" group) would clobber each other's
// page; the generator guards against silently dropping one.
func docSlugsUnique(cmds []clidocs.Command) (string, bool) {
	seen := map[string]bool{}
	for _, c := range cmds {
		if seen[c.Slug()] {
			return c.Slug(), false
		}
		seen[c.Slug()] = true
	}
	return "", true
}
