// Package clidocs renders reference documentation — roff man pages and a
// Markdown reference — from a description of the goobers CLI command tree
// (#1096, CLI-2, design docs/design/v1/cli-surface-and-manpages.md §2). It is
// deliberately decoupled from cmd/goobers' registry type (which carries func
// values and cannot be serialized or imported outside package main): the caller
// projects the registry into the plain Command values here, and these pure
// renderers turn them into deterministic, byte-stable output.
//
// Determinism is a hard requirement: the generated files are committed and a CI
// regen-diff guard fails the build on any drift (the same discipline as the
// JSON-schema parity checks). So the output must never embed a date, a build
// version, or any other volatile value — nothing here does.
package clidocs

import (
	"fmt"
	"sort"
	"strings"
)

// Command is one node of the CLI command tree, flattened: Path is the full
// invocation path below the root binary (for `goobers run abort`, Path is
// ["run","abort"]). Short is the one-line description; Long is the full `-h`
// help body (verbatim, as a user sees it); Examples are runnable invocations.
type Command struct {
	Path     []string
	Short    string
	Long     string
	Examples []string
}

// Name is the space-joined invocation path below the binary ("run abort").
func (c Command) Name() string { return strings.Join(c.Path, " ") }

// FullName is the complete invocation including the binary ("goobers run abort").
func (c Command) FullName() string {
	if len(c.Path) == 0 {
		return "goobers"
	}
	return "goobers " + c.Name()
}

// Slug is the hyphen-joined file/anchor stem ("goobers-run-abort", or "goobers"
// for the root). It is the man-page file stem and the Markdown heading anchor.
func (c Command) Slug() string {
	if len(c.Path) == 0 {
		return "goobers"
	}
	return "goobers-" + strings.Join(c.Path, "-")
}

// ManFile is the man-page file name for the command ("goobers-run-abort.1").
func (c Command) ManFile() string { return c.Slug() + ".1" }

// Sort orders commands by invocation path so generated output is stable
// regardless of registry declaration order.
func Sort(cmds []Command) {
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name() < cmds[j].Name() })
}

// ManPage renders a roff man page (section 1) for a single command.
func ManPage(c Command) string {
	var b strings.Builder
	title := strings.ToUpper(strings.ReplaceAll(c.Slug(), "-", " "))
	b.WriteString(".\\\" Generated from the goobers command registry — do not edit by hand.\n")
	// Empty date and source fields keep the output free of volatile values so
	// the committed man pages never drift on a re-run.
	fmt.Fprintf(&b, ".TH \"%s\" \"1\" \"\" \"Goobers\" \"Goobers Manual\"\n", title)

	b.WriteString(".SH NAME\n")
	name := roffEscapeInline(c.FullName())
	if c.Short != "" {
		fmt.Fprintf(&b, "%s \\- %s\n", name, roffEscapeInline(c.Short))
	} else {
		fmt.Fprintf(&b, "%s\n", name)
	}

	b.WriteString(".SH SYNOPSIS\n")
	fmt.Fprintf(&b, "\\fB%s\\fR [\\fIoptions\\fR]\n", roffEscapeInline(c.FullName()))

	b.WriteString(".SH DESCRIPTION\n")
	body := c.Long
	if strings.TrimSpace(body) == "" {
		body = c.Short
	}
	b.WriteString(roffPreformatted(body))

	if len(c.Examples) > 0 {
		b.WriteString(".SH EXAMPLES\n")
		b.WriteString(roffPreformatted(strings.Join(c.Examples, "\n")))
	}

	b.WriteString(".SH SEE ALSO\n")
	b.WriteString("\\fBgoobers\\fR(1)\n")
	return b.String()
}

// ManIndex renders the top-level goobers.1 man page: the binary's own
// description plus a one-line list of every command, cross-referencing each
// command's own page.
func ManIndex(root Command, cmds []Command) string {
	Sort(cmds)
	var b strings.Builder
	b.WriteString(".\\\" Generated from the goobers command registry — do not edit by hand.\n")
	b.WriteString(".TH \"GOOBERS\" \"1\" \"\" \"Goobers\" \"Goobers Manual\"\n")
	b.WriteString(".SH NAME\n")
	short := root.Short
	if short == "" {
		short = "tier 1-2 local instance CLI"
	}
	fmt.Fprintf(&b, "goobers \\- %s\n", roffEscapeInline(short))
	b.WriteString(".SH SYNOPSIS\n")
	b.WriteString("\\fBgoobers\\fR \\fIcommand\\fR [\\fIoptions\\fR]\n")
	if strings.TrimSpace(root.Long) != "" {
		b.WriteString(".SH DESCRIPTION\n")
		b.WriteString(roffPreformatted(root.Long))
	}
	b.WriteString(".SH COMMANDS\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, ".TP\n\\fB%s\\fR\n%s\n", roffEscapeInline(c.FullName()), roffEscapeInline(c.Short))
	}
	b.WriteString(".SH SEE ALSO\n")
	refs := make([]string, 0, len(cmds))
	for _, c := range cmds {
		refs = append(refs, fmt.Sprintf("\\fB%s\\fR(1)", roffEscapeInline(c.Slug())))
	}
	b.WriteString(strings.Join(refs, ",\n") + "\n")
	return b.String()
}

// Reference renders the single Markdown CLI reference (docs/cli/README.md): an
// intro, a command table with anchor links, then a section per command.
func Reference(root Command, cmds []Command) string {
	Sort(cmds)
	var b strings.Builder
	b.WriteString("# `goobers` CLI reference\n\n")
	b.WriteString("<!-- Generated from the command registry (cmd/goobers/runtime_capabilities.go) by `make docs`. Do not edit by hand; edits are overwritten and the CI drift guard will fail. -->\n\n")
	intro := root.Short
	if intro == "" {
		intro = "tier 1-2 local instance CLI"
	}
	fmt.Fprintf(&b, "`goobers` is the %s. This reference is generated from the CLI command registry, so it always matches the shipped binary.\n\n", intro)

	b.WriteString("## Commands\n\n")
	b.WriteString("| Command | Description |\n| --- | --- |\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "| [`%s`](#%s) | %s |\n", c.FullName(), anchor(c.Slug()), mdEscapeCell(c.Short))
	}
	b.WriteString("\n")

	for _, c := range cmds {
		fmt.Fprintf(&b, "## `%s`\n\n", c.FullName())
		if c.Short != "" {
			fmt.Fprintf(&b, "%s\n\n", c.Short)
		}
		body := c.Long
		if strings.TrimSpace(body) == "" {
			body = c.Short
		}
		// A tilde fence avoids collision with any backticks inside help bodies.
		b.WriteString("~~~text\n")
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n~~~\n\n")
		if len(c.Examples) > 0 {
			b.WriteString("**Examples**\n\n~~~console\n")
			for _, ex := range c.Examples {
				fmt.Fprintf(&b, "$ %s\n", ex)
			}
			b.WriteString("~~~\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// anchor mirrors GitHub's heading-to-anchor rule closely enough for the slugs
// this generator produces (lowercase ASCII + hyphens already), so the table
// links resolve. The command slugs contain no spaces or punctuation beyond the
// hyphens, so lowercasing is sufficient.
func anchor(slug string) string { return strings.ToLower(slug) }

// roffEscapeInline escapes a string for use inside a roff text line: the escape
// character itself must be doubled so it renders literally.
func roffEscapeInline(s string) string {
	return strings.ReplaceAll(s, "\\", "\\e")
}

// roffPreformatted renders a multi-line body as a no-fill (.nf/.fi) block so the
// exact `-h` layout is preserved. Each line is escaped, and a line that begins
// with a roff control character (. or ') is prefixed with \& so it is not
// interpreted as a request.
func roffPreformatted(body string) string {
	var b strings.Builder
	b.WriteString(".nf\n")
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		line = roffEscapeInline(line)
		if strings.HasPrefix(line, ".") || strings.HasPrefix(line, "'") {
			line = "\\&" + line
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(".fi\n")
	return b.String()
}

// mdEscapeCell makes a short description safe inside a Markdown table cell: a
// literal pipe would otherwise start a new column, and newlines would break the
// row. Descriptions are one-liners, so this only has to defend those two.
func mdEscapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
