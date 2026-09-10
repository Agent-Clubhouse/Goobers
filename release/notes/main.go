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
	"strconv"
	"strings"
)

var (
	stableTagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	// tagPattern additionally accepts a SemVer 2.0.0 pre-release suffix
	// (e.g. "v1.2.3-beta.2", "v1.2.3-rc.1") for tags this generator can
	// render notes for. previousTag still filters candidates with
	// stableTagPattern: the "changes since" anchor and the release
	// workflow's feature/support-matrix baseline always diff against the
	// last stable release, never another pre-release.
	tagPattern         = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)
	conventionalCommit = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*)(?:\(([^)]+)\))?(!)?:[[:space:]]+(.+)$`)
	breakingFooter     = regexp.MustCompile(`(?m)^BREAKING(?: CHANGE|-CHANGE):[ \t]*(.+)$`)

	// curatedCommitCountPattern matches a curated note's own claim about how
	// many commits it covers (#4272), e.g. "58 commits since v0.4.0-beta.1".
	// A curated note that makes no such claim (the current convention: a
	// prose "Highlights" list with no counts) has nothing to validate.
	curatedCommitCountPattern = regexp.MustCompile(`(?i)(\d+)\s+commits?\s+since\s+(\S+)`)
	// curatedKindCountPattern matches an optional per-Conventional-Commit-type
	// breakdown in the same claim, e.g. "(20 fix, 6 refactor, ...)".
	curatedKindCountPattern = regexp.MustCompile(`(?i)(\d+)\s+(feat|fix|perf|docs|refactor|test|build|ci|chore|revert)\b`)
)

const gitLogFormat = "%H%x1f%B%x1e"

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
	hash          string
	kind          string
	scope         string
	description   string
	subject       string
	breakingNotes []string
	breaking      bool
	conventional  bool
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
	tag := fs.String("tag", "", "release tag (vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-prerelease)")
	featureNotes := fs.String("feature-notes", "", "generated feature-policy notes to append")
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
	if *featureNotes != "" {
		generated, err := readFile(*featureNotes)
		if err != nil {
			return fmt.Errorf("read feature notes %s: %w", *featureNotes, err)
		}
		notes, err = combineFeatureNotes(notes, string(generated))
		if err != nil {
			return fmt.Errorf("combine feature notes %s: %w", *featureNotes, err)
		}
	}
	notes, err = boundReleaseNotes(notes)
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

// GitHub rejects release bodies over 125,000 characters with an opaque
// HTTP 422 at PUBLISH time — after every artifact is built and signed. The
// v0.2.0 release hit exactly that: the #3292 registry backfill grew the
// inlined DSL delta tables past the limit and the publish step died as the
// last act of an otherwise green pipeline. maxReleaseNotesLen keeps headroom
// under the API limit so the bound fails at BUILD time or trims at build
// time, never at publish.
const maxReleaseNotesLen = 120000

// deltaSections are the generated, unbounded-growth sections whose table rows
// may be trimmed to fit; every other section is authored prose and is never
// touched. The complete tables always ship in the release docs bundle
// (docs/feature-matrix.md), so trimming here loses no information.
var deltaSections = []string{
	"## DSL feature-support delta",
	"## DSL support-matrix delta",
}

// boundReleaseNotes enforces maxReleaseNotesLen. Oversized notes lose table
// rows from the bottom of the largest delta section first, each trimmed
// section gaining an explicit omission line; if the notes still cannot fit
// (the overflow is outside the trimmable sections), it errors loudly so the
// release fails before an artifact is published against it.
func boundReleaseNotes(notes string) (string, error) {
	if len(notes) <= maxReleaseNotesLen {
		return notes, nil
	}
	for len(notes) > maxReleaseNotesLen {
		section, rows := largestTrimmableDelta(notes)
		if section == "" || rows == 0 {
			return "", fmt.Errorf(
				"release notes are %d characters (limit %d) and no delta table rows remain to trim — shorten the authored sections",
				len(notes), maxReleaseNotesLen)
		}
		trim := (len(notes) - maxReleaseNotesLen + rowLen(notes, section) - 1) / rowLen(notes, section)
		if trim < 1 {
			trim = 1
		}
		if trim > rows {
			trim = rows
		}
		notes = trimDeltaRows(notes, section, trim)
	}
	return notes, nil
}

// sectionBounds returns the [start, end) of the named section, where end is
// the next "\n## " heading or the end of the notes.
func sectionBounds(notes, heading string) (int, int) {
	start := strings.Index(notes, heading)
	if start < 0 {
		return -1, -1
	}
	rest := notes[start+len(heading):]
	next := strings.Index(rest, "\n## ")
	if next < 0 {
		return start, len(notes)
	}
	return start, start + len(heading) + next + 1
}

// tableRowIndexes returns the offsets (relative to notes) of each markdown
// table BODY row in the section — lines starting with "| " excluding the
// header and separator rows.
func tableRowIndexes(notes, heading string) []int {
	start, end := sectionBounds(notes, heading)
	if start < 0 {
		return nil
	}
	var rows []int
	seenHeader := false
	seenSeparator := false
	offset := start
	for _, line := range strings.SplitAfter(notes[start:end], "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "|") {
			switch {
			case !seenHeader:
				seenHeader = true
			case !seenSeparator:
				seenSeparator = true
			default:
				rows = append(rows, offset)
			}
		}
		offset += len(line)
	}
	return rows
}

func largestTrimmableDelta(notes string) (string, int) {
	best, bestRows := "", 0
	for _, heading := range deltaSections {
		if n := len(tableRowIndexes(notes, heading)); n > bestRows {
			best, bestRows = heading, n
		}
	}
	return best, bestRows
}

// rowLen estimates the byte cost of one table row in the section (its last
// row's length), used to compute how many rows must go.
func rowLen(notes, heading string) int {
	rows := tableRowIndexes(notes, heading)
	if len(rows) == 0 {
		return 1
	}
	last := rows[len(rows)-1]
	lineEnd := strings.IndexByte(notes[last:], '\n')
	if lineEnd < 0 {
		lineEnd = len(notes) - last
	}
	if lineEnd < 1 {
		return 1
	}
	return lineEnd + 1
}

// trimDeltaRows removes the last n table rows of the section and appends an
// omission line accounting for every row trimmed from it so far.
func trimDeltaRows(notes, heading string, n int) string {
	rows := tableRowIndexes(notes, heading)
	if n > len(rows) {
		n = len(rows)
	}
	cut := rows[len(rows)-n]
	_, end := sectionBounds(notes, heading)
	tail := notes[end:]
	head := notes[:cut]

	const omissionPrefix = "_…delta truncated: "
	already := 0
	if idx := strings.LastIndex(head, omissionPrefix); idx >= 0 && idx > strings.Index(head, heading) {
		lineEnd := strings.IndexByte(head[idx:], '\n')
		var line string
		if lineEnd < 0 {
			line = head[idx:]
			head = head[:idx]
		} else {
			line = head[idx : idx+lineEnd]
			head = head[:idx]
		}
		if _, err := fmt.Sscanf(line, "_…delta truncated: %d", &already); err != nil {
			already = 0 // unparsable prior omission line: recount from zero rather than guess
		}
	}
	omission := fmt.Sprintf(
		"%s%d rows omitted to fit GitHub's release-body limit; the complete matrix ships in the release docs bundle (docs/feature-matrix.md)._\n",
		omissionPrefix, already+n)
	return strings.TrimRight(head, "\n") + "\n" + omission + tail
}

func combineFeatureNotes(changelog, generated string) (string, error) {
	const featureSection = "## DSL feature-support delta"
	start := strings.Index(generated, featureSection)
	if start < 0 {
		return "", fmt.Errorf("missing %q section", featureSection)
	}
	return strings.TrimRight(changelog, "\n") + "\n\n" +
		strings.TrimSpace(generated[start:]) + "\n", nil
}

func generate(tag string, git gitClient, readFile func(string) ([]byte, error)) (string, error) {
	if !tagPattern.MatchString(tag) {
		return "", fmt.Errorf("tag %q is not a valid release tag (want vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-prerelease)", tag)
	}
	if _, err := git.output("rev-parse", "--verify", "refs/tags/"+tag+"^{commit}"); err != nil {
		return "", fmt.Errorf("resolve release tag %s: %w", tag, err)
	}

	curated, err := curatedNote(tag, git, readFile)
	if err != nil {
		return "", err
	}
	if err := validateCuratedCounts(curated, tag, git); err != nil {
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

// validateCuratedCounts fails loudly when a curated note's own commit-count
// claim disagrees with the real range (#4272): a release re-tagged after a
// failed attempt at an earlier commit otherwise leaves a stale, curated
// claim that nothing else in the pipeline re-derives or checks — the one
// paragraph a reader trusts most because it was hand-authored, describing
// commits that were never actually released. A curated note that states no
// count (the current convention: a prose "Highlights" list) has nothing to
// validate; this is a defense against a stated claim going stale, not a
// requirement to make one.
func validateCuratedCounts(curated, tag string, git gitClient) error {
	match := curatedCommitCountPattern.FindStringSubmatchIndex(curated)
	if match == nil {
		return nil
	}
	statedCount, err := strconv.Atoi(curated[match[2]:match[3]])
	if err != nil {
		return nil // the pattern only captures digits; unreachable in practice
	}
	// (\S+) is greedy, so it captures the whole token up to whitespace —
	// trim trailing sentence/parenthetical punctuation a version-like token
	// never legitimately ends with (its own internal '.' separators must
	// survive, unlike a "since v0.4.0-beta.1)" or "...beta.1." trailer).
	ref := strings.TrimRight(curated[match[4]:match[5]], ",.();:")
	revision := ref + ".." + tag
	out, err := git.output("rev-list", "--count", revision)
	if err != nil {
		return fmt.Errorf("verify curated note's commit-count claim (%s): %w", revision, err)
	}
	actualCount, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return fmt.Errorf("git rev-list --count %s returned non-numeric output %q", revision, out)
	}
	if statedCount != actualCount {
		return fmt.Errorf(
			"curated release note claims %d commits since %s, but git rev-list --count %s reports %d — "+
				"the curated note was written against a different commit (likely a re-tag after a failed run); regenerate it for the commit that actually shipped",
			statedCount, ref, revision, actualCount)
	}

	// Narrow the per-type breakdown search to the claim's own sentence (up to
	// the next '.' or end of string) so unrelated prose elsewhere in the
	// curated note cannot spuriously match an "N fix"-shaped phrase.
	sentence := curated[match[1]:]
	if end := strings.IndexByte(sentence, '.'); end >= 0 {
		sentence = sentence[:end]
	}
	kindMatches := curatedKindCountPattern.FindAllStringSubmatch(sentence, -1)
	if len(kindMatches) == 0 {
		return nil
	}
	logOutput, err := git.output("log", "--format=%s", revision)
	if err != nil {
		return fmt.Errorf("verify curated note's per-type commit counts (%s): %w", revision, err)
	}
	actualKindCounts := map[string]int{}
	for _, subject := range strings.Split(logOutput, "\n") {
		subject = strings.TrimSpace(subject)
		if subject == "" {
			continue
		}
		if m := conventionalCommit.FindStringSubmatch(subject); m != nil {
			actualKindCounts[strings.ToLower(m[1])]++
		}
	}
	for _, kindMatch := range kindMatches {
		statedKindCount, err := strconv.Atoi(kindMatch[1])
		if err != nil {
			continue
		}
		kindName := strings.ToLower(kindMatch[2])
		if statedKindCount != actualKindCounts[kindName] {
			return fmt.Errorf(
				"curated release note claims %d %q commits since %s, but %s has %d — "+
					"the curated note was written against a different commit (likely a re-tag after a failed run); regenerate it for the commit that actually shipped",
				statedKindCount, kindName, ref, revision, actualKindCounts[kindName])
		}
	}
	return nil
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
		if message = strings.TrimSpace(message); message != "" {
			return message, nil
		}
	}
	return "", fmt.Errorf(
		"curated release note is required: add non-empty %s or use an annotated tag with a non-empty message",
		path,
	)
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
	log, err := git.output("log", "--first-parent", "--format="+gitLogFormat, revision)
	if err != nil {
		return nil, fmt.Errorf("read changelog history %s: %w", revision, err)
	}
	if log == "" {
		return nil, nil
	}

	var changes []change
	for record := range strings.SplitSeq(log, "\x1e") {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		hash, message, ok := strings.Cut(record, "\x1f")
		hash = strings.TrimSpace(hash)
		message = strings.TrimSpace(message)
		if !ok || hash == "" || message == "" {
			return nil, fmt.Errorf("parse git log record %q", record)
		}
		subject, body, _ := strings.Cut(message, "\n")
		subject = strings.TrimSpace(strings.TrimSuffix(subject, "\r"))
		item := change{hash: hash, subject: subject}
		if match := conventionalCommit.FindStringSubmatch(subject); match != nil {
			item.kind = strings.ToLower(match[1])
			item.scope = match[2]
			item.breaking = match[3] == "!"
			item.description = match[4]
			item.conventional = true
		}
		for _, match := range breakingFooter.FindAllStringSubmatch(body, -1) {
			if note := strings.TrimSpace(match[1]); note != "" {
				item.breakingNotes = append(item.breakingNotes, note)
			}
		}
		item.breaking = item.breaking || len(item.breakingNotes) > 0
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
	if len(item.breakingNotes) > 0 {
		description += " (" + strings.Join(item.breakingNotes, "; ") + ")"
	}
	hash := item.hash
	if len(hash) > 7 {
		hash = hash[:7]
	}
	fmt.Fprintf(out, "- %s (`%s`)\n", description, hash)
}
