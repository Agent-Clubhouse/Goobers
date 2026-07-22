// Command notes generates GitHub Release notes from a curated note and the
// Conventional-Commit history since the previous stable release tag.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	stableTagPattern   = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	conventionalCommit = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*)(?:\(([^)]+)\))?(!)?:[[:space:]]+(.+)$`)
)

type gitClient interface {
	output(args ...string) (string, error)
}

type execGit struct{}

func (execGit) output(args ...string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

type change struct {
	hash         string
	kind         string
	scope        string
	description  string
	subject      string
	breaking     bool
	conventional bool
}

type section struct {
	title string
	match func(change) bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, execGit{}, os.ReadFile); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "release notes:", err)
		os.Exit(1)
	}
}

func run(
	args []string,
	stdout, stderr io.Writer,
	git gitClient,
	readFile func(string) ([]byte, error),
) error {
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tag := fs.String("tag", "", "stable release tag (vMAJOR.MINOR.PATCH)")
	output := fs.String("output", "", "write notes to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	notes, err := generate(*tag, git, readFile)
	if err != nil {
		return err
	}
	if *output == "" {
		_, err = io.WriteString(stdout, notes)
		return err
	}
	if err := os.WriteFile(*output, []byte(notes), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *output, err)
	}
	return nil
}

func generate(tag string, git gitClient, readFile func(string) ([]byte, error)) (string, error) {
	if !stableTagPattern.MatchString(tag) {
		return "", fmt.Errorf("tag %q is not a stable semantic version (want vMAJOR.MINOR.PATCH)", tag)
	}
	if _, err := git.output("rev-parse", "--verify", "refs/tags/"+tag+"^{commit}"); err != nil {
		return "", fmt.Errorf("resolve release tag %s: %w", tag, err)
	}

	curated, err := curatedNote(tag, git, readFile)
	if err != nil {
		return "", err
	}
	previous, err := previousTag(tag, git)
	if err != nil {
		return "", err
	}
	changes, err := changesSince(tag, previous, git)
	if err != nil {
		return "", err
	}
	return render(tag, previous, curated, changes), nil
}

func curatedNote(tag string, git gitClient, readFile func(string) ([]byte, error)) (string, error) {
	path := filepath.Join(".github", "release-notes", tag+".md")
	note, err := readFile(path)
	switch {
	case err == nil && strings.TrimSpace(string(note)) != "":
		return strings.TrimSpace(string(note)), nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("read curated note %s: %w", path, err)
	}

	objectType, err := git.output("cat-file", "-t", "refs/tags/"+tag)
	if err != nil {
		return "", fmt.Errorf("inspect release tag %s: %w", tag, err)
	}
	if objectType == "tag" {
		message, err := git.output("for-each-ref", "--format=%(contents)", "refs/tags/"+tag)
		if err != nil {
			return "", fmt.Errorf("read annotated tag %s: %w", tag, err)
		}
		if message != "" {
			return message, nil
		}
	}
	return "This release includes the changes listed below.", nil
}

func previousTag(tag string, git gitClient) (string, error) {
	line, err := git.output("rev-list", "--parents", "-n", "1", tag)
	if err != nil {
		return "", fmt.Errorf("inspect release commit %s: %w", tag, err)
	}
	if len(strings.Fields(line)) == 1 {
		return "", nil
	}

	tags, err := git.output("tag", "--merged", tag+"^", "--sort=-version:refname", "--list", "v*")
	if err != nil {
		return "", fmt.Errorf("find previous release tag: %w", err)
	}
	for candidate := range strings.Lines(tags) {
		candidate = strings.TrimSpace(candidate)
		if stableTagPattern.MatchString(candidate) {
			return candidate, nil
		}
	}
	return "", nil
}

func changesSince(tag, previous string, git gitClient) ([]change, error) {
	revision := tag
	if previous != "" {
		revision = previous + ".." + tag
	}
	log, err := git.output("log", "--first-parent", "--format=%H%x09%s", revision)
	if err != nil {
		return nil, fmt.Errorf("read changelog history %s: %w", revision, err)
	}
	if log == "" {
		return nil, nil
	}

	var changes []change
	for line := range strings.Lines(log) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		hash, subject, ok := strings.Cut(line, "\t")
		if !ok || hash == "" || subject == "" {
			return nil, fmt.Errorf("parse git log line %q", line)
		}
		item := change{hash: hash, subject: subject}
		if match := conventionalCommit.FindStringSubmatch(subject); match != nil {
			item.kind = strings.ToLower(match[1])
			item.scope = match[2]
			item.breaking = match[3] == "!"
			item.description = match[4]
			item.conventional = true
		}
		changes = append(changes, item)
	}
	return changes, nil
}

func render(tag, previous, curated string, changes []change) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n%s\n\n## Changelog\n\n", tag, curated)
	if previous == "" {
		out.WriteString("Initial release.\n")
	} else {
		fmt.Fprintf(&out, "Changes since `%s`.\n", previous)
	}

	sections := []section{
		{title: "Breaking changes", match: func(c change) bool { return c.breaking }},
		{title: "Features", match: kind("feat")},
		{title: "Bug fixes", match: kind("fix")},
		{title: "Performance", match: kind("perf")},
		{title: "Documentation", match: kind("docs")},
		{title: "Refactoring", match: kind("refactor")},
		{title: "Tests", match: kind("test")},
		{title: "Build", match: kind("build")},
		{title: "CI", match: kind("ci")},
		{title: "Maintenance", match: kind("chore")},
		{title: "Reverts", match: kind("revert")},
		{title: "Other changes", match: func(c change) bool {
			switch c.kind {
			case "feat", "fix", "perf", "docs", "refactor", "test", "build", "ci", "chore", "revert":
				return !c.conventional
			default:
				return !c.breaking
			}
		}},
	}

	wroteSection := false
	for _, group := range sections {
		var entries []change
		for _, item := range changes {
			if group.match(item) {
				entries = append(entries, item)
			}
		}
		if len(entries) == 0 {
			continue
		}
		wroteSection = true
		fmt.Fprintf(&out, "\n### %s\n\n", group.title)
		for _, item := range entries {
			writeChange(&out, item)
		}
	}
	if !wroteSection {
		out.WriteString("\nNo commits are included in this release.\n")
	}
	return out.String()
}

func kind(want string) func(change) bool {
	return func(c change) bool { return c.conventional && !c.breaking && c.kind == want }
}

func writeChange(out *strings.Builder, item change) {
	description := item.subject
	if item.conventional {
		description = item.description
		if item.scope != "" {
			description = fmt.Sprintf("**%s:** %s", item.scope, description)
		}
	}
	hash := item.hash
	if len(hash) > 7 {
		hash = hash[:7]
	}
	fmt.Fprintf(out, "- %s (`%s`)\n", description, hash)
}
