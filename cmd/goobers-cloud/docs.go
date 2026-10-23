package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/clidocs"
	"github.com/goobers/goobers/internal/cliregistry"
)

const completionHelp = `Usage: goobers-cloud completion <bash|zsh|fish>

Generate a shell completion script. Source the output in the target shell.
`

func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 {
		switch args[0] {
		case "bash", "zsh", "fish":
			write(stdout, completionScript(args[0]))
			return 0
		}
	}
	write(stderr, completionHelp)
	return 2
}

func documentedCommands() []clidocs.Command {
	var docs []clidocs.Command
	for _, candidate := range commands {
		if candidate.hidden || candidate.name == "help" {
			continue
		}
		docs = append(docs, clidocs.Command{
			Binary:   "goobers-cloud",
			Path:     strings.Fields(candidate.name),
			Short:    candidate.short,
			Long:     candidate.long,
			Examples: candidate.examples,
			Group:    clidocs.GroupCore,
		})
	}
	clidocs.Sort(docs)
	return docs
}

func renderDocs() map[string]string {
	root := clidocs.Command{
		Binary: "goobers-cloud",
		Short:  "cloud control-plane role host",
		Long:   "Run one cloud control-plane role per process.",
	}
	docs := documentedCommands()
	files := map[string]string{
		"cloud-cli/README.md":           clidocs.Reference(root, docs),
		"completion/goobers-cloud.bash": completionScript("bash"),
		"completion/goobers-cloud.fish": completionScript("fish"),
		"completion/_goobers-cloud":     completionScript("zsh"),
		"man/goobers-cloud.1":           clidocs.ManIndex(root, docs),
	}
	for _, command := range docs {
		files["man/"+command.ManFile()] = clidocs.ManPage(command)
	}
	return files
}

func completionScript(shell string) string {
	paths := make([]string, 0, len(commands))
	for _, candidate := range commands {
		if !candidate.hidden {
			paths = append(paths, candidate.name)
		}
	}
	return cliregistry.Completion("goobers-cloud", shell, paths)
}

func writeDocs(root string) error {
	for relative, content := range renderDocs() {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func runGenerateDocs(args []string, _ io.Writer, stderr io.Writer) int {
	if len(args) != 1 {
		writeln(stderr, "usage: goobers-cloud __generate-docs <docs-directory>")
		return 2
	}
	if err := writeDocs(args[0]); err != nil {
		writef(stderr, "generate docs: %v\n", err)
		return 1
	}
	return 0
}
