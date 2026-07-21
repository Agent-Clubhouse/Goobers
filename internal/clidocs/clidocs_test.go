package clidocs

import (
	"strings"
	"testing"
)

func TestCommandNaming(t *testing.T) {
	root := Command{}
	if root.FullName() != "goobers" || root.Slug() != "goobers" {
		t.Errorf("root naming = %q/%q", root.FullName(), root.Slug())
	}
	nested := Command{Path: []string{"run", "abort"}}
	if nested.Name() != "run abort" {
		t.Errorf("Name = %q", nested.Name())
	}
	if nested.FullName() != "goobers run abort" {
		t.Errorf("FullName = %q", nested.FullName())
	}
	if nested.Slug() != "goobers-run-abort" {
		t.Errorf("Slug = %q", nested.Slug())
	}
	if nested.ManFile() != "goobers-run-abort.1" {
		t.Errorf("ManFile = %q", nested.ManFile())
	}
}

func TestManPageRoffEscaping(t *testing.T) {
	page := ManPage(Command{
		Path:  []string{"init"},
		Short: `scaffold a\path`,
		Long:  ".TH is a control line\nplain line\n'apostrophe control\n",
	})
	// The escape character is doubled so a literal backslash renders.
	if !strings.Contains(page, `scaffold a\epath`) {
		t.Errorf("backslash not escaped in NAME:\n%s", page)
	}
	// Lines beginning with a roff control char are protected with \&.
	if !strings.Contains(page, `\&.TH is a control line`) {
		t.Errorf("leading dot not protected:\n%s", page)
	}
	if !strings.Contains(page, `\&'apostrophe control`) {
		t.Errorf("leading apostrophe not protected:\n%s", page)
	}
	if !strings.Contains(page, ".nf\nplain line") && !strings.Contains(page, "plain line") {
		t.Errorf("plain line missing:\n%s", page)
	}
}

func TestManPageOmitsExamplesWhenNone(t *testing.T) {
	page := ManPage(Command{Path: []string{"x"}, Short: "s", Long: "body"})
	if strings.Contains(page, ".SH EXAMPLES") {
		t.Errorf("EXAMPLES section should be omitted when there are no examples:\n%s", page)
	}
}

func TestReferenceMarkdown(t *testing.T) {
	cmds := []Command{
		{Path: []string{"beta"}, Short: "second | with pipe", Long: "beta body", Examples: []string{"goobers beta"}},
		{Path: []string{"alpha"}, Short: "first", Long: "alpha body"},
	}
	md := Reference(Command{Short: "test CLI"}, cmds)

	// Sorted: alpha before beta regardless of input order.
	if strings.Index(md, "alpha") > strings.Index(md, "## `goobers beta`") {
		t.Errorf("commands not sorted:\n%s", md)
	}
	// TOC link + anchor.
	if !strings.Contains(md, "[`goobers alpha`](#goobers-alpha)") {
		t.Errorf("missing TOC link:\n%s", md)
	}
	// Per-command section heading (drives the anchor).
	if !strings.Contains(md, "## `goobers beta`") {
		t.Errorf("missing command section:\n%s", md)
	}
	// Pipe in a table cell is escaped so it doesn't split the column.
	if !strings.Contains(md, `second \| with pipe`) {
		t.Errorf("pipe not escaped in table cell:\n%s", md)
	}
	// Examples rendered as a console block.
	if !strings.Contains(md, "$ goobers beta") {
		t.Errorf("example not rendered:\n%s", md)
	}
}

func TestRenderersAreDeterministic(t *testing.T) {
	// Distinct input slices with the same commands in different orders must
	// render identically — the renderers sort internally, so registry
	// declaration order cannot perturb the committed output.
	forward := []Command{
		{Path: []string{"a"}, Short: "a", Examples: []string{"goobers a"}},
		{Path: []string{"b"}, Short: "b"},
	}
	reversed := []Command{forward[1], forward[0]}
	root := Command{Short: "cli"}

	if got, other := Reference(root, forward), Reference(root, reversed); got != other {
		t.Errorf("Reference depends on input order:\n%s\n---\n%s", got, other)
	}
	if got, other := ManIndex(root, forward), ManIndex(root, reversed); got != other {
		t.Errorf("ManIndex depends on input order:\n%s\n---\n%s", got, other)
	}
}
