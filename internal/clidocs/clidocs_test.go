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

func TestIsWorkflowStage(t *testing.T) {
	cases := []struct {
		short string
		want  bool
	}{
		{"open or update the run's PR (a workflow stage)", true},
		{"emit the docs-drift churn digest since the watermark (a connector stage)", true},
		{"trigger a run manually (still honors run conditions)", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsWorkflowStage(c.short); got != c.want {
			t.Errorf("IsWorkflowStage(%q) = %v, want %v", c.short, got, c.want)
		}
	}
}

// TestReferenceSplitsCoreAndStageCommands is #2012's core tiering guard: a
// command marked as a workflow/connector stage appears only under the
// "Workflow-stage and connector commands" table, never the "Commands" table,
// and vice versa.
func TestReferenceSplitsCoreAndStageCommands(t *testing.T) {
	cmds := []Command{
		{Path: []string{"init"}, Short: "scaffold an instance root"},
		{Path: []string{"open-pr"}, Short: "open or update the run's PR (a workflow stage)"},
	}
	md := Reference(Command{Short: "test CLI"}, cmds)

	coreSection := md[strings.Index(md, "## Commands\n"):strings.Index(md, "## Workflow-stage and connector commands\n")]
	stageSection := md[strings.Index(md, "## Workflow-stage and connector commands\n"):strings.Index(md, "## `goobers init`")]

	if !strings.Contains(coreSection, "goobers-init") {
		t.Errorf("core table missing init:\n%s", coreSection)
	}
	if strings.Contains(coreSection, "goobers-open-pr") {
		t.Errorf("core table must not list the stage command open-pr:\n%s", coreSection)
	}
	if !strings.Contains(stageSection, "goobers-open-pr") {
		t.Errorf("stage table missing open-pr:\n%s", stageSection)
	}
	if strings.Contains(stageSection, "goobers-init") {
		t.Errorf("stage table must not list the core command init:\n%s", stageSection)
	}
}

func TestManIndexSplitsCoreAndStageCommands(t *testing.T) {
	cmds := []Command{
		{Path: []string{"init"}, Short: "scaffold an instance root"},
		{Path: []string{"open-pr"}, Short: "open or update the run's PR (a workflow stage)"},
	}
	man := ManIndex(Command{Short: "test CLI"}, cmds)

	coreSection := man[strings.Index(man, ".SS Core commands\n"):strings.Index(man, ".SS Workflow-stage and connector commands\n")]
	stageSection := man[strings.Index(man, ".SS Workflow-stage and connector commands\n"):]

	if !strings.Contains(coreSection, "goobers init") {
		t.Errorf("core section missing init:\n%s", coreSection)
	}
	if strings.Contains(coreSection, "goobers open-pr") {
		t.Errorf("core section must not list the stage command open-pr:\n%s", coreSection)
	}
	if !strings.Contains(stageSection, "goobers open-pr") {
		t.Errorf("stage section missing open-pr:\n%s", stageSection)
	}
	if strings.Contains(stageSection, "goobers init") {
		t.Errorf("stage section must not list the core command init:\n%s", stageSection)
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
