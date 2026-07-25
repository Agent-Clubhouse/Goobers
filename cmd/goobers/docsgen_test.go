package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/clidocs"
)

// docsDir resolves the repository docs/ directory from the package directory
// (cmd/goobers -> ../../docs).
func docsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "docs"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCLIDocsUpToDate is the regen-diff guard (#1096): the committed man pages
// and Markdown reference must match what the generator produces from the
// registry, byte for byte, so the docs cannot drift from the shipped CLI. When
// a CLI change is intentional, regenerate with
// UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestCLIDocsUpToDate (or `make docs`).
func TestCLIDocsUpToDate(t *testing.T) {
	unsetRunContext(t)
	dir := docsDir(t)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := writeCLIDocs(dir); err != nil {
			t.Fatalf("writeCLIDocs: %v", err)
		}
		return
	}

	want := renderCLIDocs()

	// Every generated file matches its committed copy.
	for rel, content := range want {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v\n\nCLI docs are missing or stale — regenerate with UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestCLIDocsUpToDate", rel, err)
		}
		if string(got) != content {
			t.Fatalf("CLI doc %s is out of date; regenerate with UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestCLIDocsUpToDate", rel)
		}
	}

	// No stale generated file is committed that the generator no longer produces
	// (a page for a since-removed command).
	for _, sub := range []string{"man", "cli", "completion"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", sub, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(sub, e.Name()))
			if !isGeneratedDocFile(rel) {
				continue
			}
			if _, ok := want[rel]; !ok {
				t.Fatalf("stale committed CLI doc %s has no source command; regenerate with UPDATE_GOLDEN=1 go test ./cmd/goobers -run TestCLIDocsUpToDate", rel)
			}
		}
	}
}

// TestCLIDocsCoverEveryDocumentedCommand asserts the generator emits a man page
// for every user-facing registry command (one with a short description), and
// that no two commands collide on a man-page slug.
func TestCLIDocsCoverEveryDocumentedCommand(t *testing.T) {
	cmds := collectDocCommands(cliCommands, nil)
	if len(cmds) == 0 {
		t.Fatal("no documentable commands collected from the registry")
	}
	if slug, ok := docSlugsUnique(cmds); !ok {
		t.Fatalf("duplicate man-page slug %q — two commands would clobber each other's page", slug)
	}

	files := renderCLIDocs()
	for _, c := range cmds {
		if _, ok := files["man/"+c.ManFile()]; !ok {
			t.Errorf("command %q has no generated man page", c.FullName())
		}
		if !strings.Contains(files["cli/README.md"], "## `"+c.FullName()+"`") {
			t.Errorf("command %q missing from the Markdown reference", c.FullName())
		}
	}

	// Spot-check a representative command is fully covered, and that a hidden
	// entrypoint is not.
	var haveInit bool
	for _, c := range cmds {
		if c.Name() == "init" {
			haveInit = true
		}
		if strings.HasPrefix(c.Name(), "__") || c.Name() == detachedRunWorkerCommand {
			t.Errorf("hidden entrypoint %q leaked into the docs", c.Name())
		}
	}
	if !haveInit {
		t.Error("expected `goobers init` among documented commands")
	}
}

// TestWriteCLIDocsWritesAndPrunes exercises the generator's writer directly (the
// path `make docs` takes): it writes the full tree into a fresh directory, and a
// second run removes a stale generated page while leaving a hand-authored file
// untouched.
func TestWriteCLIDocsWritesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	if err := writeCLIDocs(dir); err != nil {
		t.Fatalf("writeCLIDocs (initial): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "man", "goobers-init.1")); err != nil {
		t.Fatalf("expected goobers-init.1 to be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cli", "README.md")); err != nil {
		t.Fatalf("expected cli/README.md to be written: %v", err)
	}
	for rel, want := range map[string]string{
		"completion/goobers.bash": bashCompletion(),
		"completion/goobers.fish": fishCompletion(),
		"completion/_goobers":     zshCompletion(),
	} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("expected %s to be written: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s differs from registry-rendered completion", rel)
		}
	}

	// A stale generated man page (a since-removed command) is pruned on the next
	// write; a hand-authored, non-generated doc alongside it is preserved.
	stale := filepath.Join(dir, "man", "goobers-gone.1")
	if err := os.WriteFile(stale, []byte(".TH stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "cli", "guide.md")
	if err := os.WriteFile(keep, []byte("# hand-authored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeCLIDocs(dir); err != nil {
		t.Fatalf("writeCLIDocs (rewrite): %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale generated page not pruned (err=%v)", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("hand-authored doc was wrongly pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "man", "goobers-init.1")); err != nil {
		t.Errorf("real page missing after rewrite: %v", err)
	}
}

func TestGenerateDocsCommand(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := runArgs(t, "__generate-docs", dir)
	if code != 0 {
		t.Fatalf("__generate-docs exit = %d, stderr = %s", code, stderr)
	}
	for _, rel := range []string{
		"cli/README.md",
		"completion/goobers.bash",
		"completion/goobers.fish",
		"completion/_goobers",
		"man/goobers.1",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("__generate-docs did not write %s: %v", rel, err)
		}
	}

	code, _, stderr = runArgs(t, "__generate-docs")
	if code != 2 || !strings.Contains(stderr, "usage:") {
		t.Fatalf("__generate-docs without directory = %d, stderr = %q", code, stderr)
	}
}

// TestManPageIsWellFormedRoff checks the structural roff a downstream man(1)
// reader relies on: the .TH header and the standard sections.
func TestManPageIsWellFormedRoff(t *testing.T) {
	page := clidocs.ManPage(clidocs.Command{
		Path:     []string{"run", "abort"},
		Short:    "mark a stuck non-terminal run aborted",
		Long:     "Usage: goobers run abort <run-id> [path]\n\n.leading dot must be escaped\n",
		Examples: []string{"goobers run abort abc123"},
	})
	for _, want := range []string{
		`.TH "GOOBERS RUN ABORT" "1"`,
		".SH NAME\ngoobers run abort \\- mark a stuck",
		".SH SYNOPSIS",
		".SH DESCRIPTION",
		".SH EXAMPLES",
		".SH SEE ALSO",
		"\\&.leading dot must be escaped", // control-line escaping
	} {
		if !strings.Contains(page, want) {
			t.Errorf("man page missing %q\n---\n%s", want, page)
		}
	}
}
