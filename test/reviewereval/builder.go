package main

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// minCaseConfidence is the share of a fix's traced lines its top
	// candidate must hold; below it the culprit is too ambiguous to score.
	minCaseConfidence = 0.5
	// maxRefactorRounds bounds how many consecutive pure-refactor commits one
	// line is traced through.
	maxRefactorRounds = 3
	defaultMaxFixes   = 500
)

// Fix sources, strongest first.
const (
	fixSourceListed     = "listed"
	fixSourceBugIssue   = "fixes-bug-issue"
	fixSourceConvention = "conventional-fix"
)

var (
	issueReference  = regexp.MustCompile(`(?i)\b(?:fix(?:e[sd])?|close[sd]?|resolve[sd]?)\s*:?\s+#(\d+)`)
	conventionalFix = regexp.MustCompile(`^fix(\([^)]*\))?!?:`)
)

// buildOptions selects which commits in Range count as bug fixes: commits
// listed explicitly (for example those recorded with
// `goobers telemetry mark-fix`), commits whose message closes an issue in
// BugIssues (issues labeled bug or flake), and, unless disabled,
// conventional `fix:` commits.
type buildOptions struct {
	Range          string
	Version        string
	BugIssues      map[int]bool
	ListedFixes    []string
	NoConventional bool
	MaxFixes       int
	HandLabels     []handLabel
}

type fixCommit struct {
	SHA     string
	Parent  string
	Subject string
	Source  string
	Issues  []int
}

// builder traces fixes to their introducing commits through a read-only view
// of the live repository.
type builder struct {
	git      gitReader
	refactor map[string]bool
	// refactorFiles holds each pure-refactor commit's parsed diff.
	refactorFiles map[string][]diffFile
	changed       map[string]map[string]bool
}

func newBuilder(repo string) *builder {
	return &builder{git: gitReader{dir: repo}, refactor: map[string]bool{}, refactorFiles: map[string][]diffFile{}, changed: map[string]map[string]bool{}}
}

// build traces every fix in the range with R-SZZ and returns the dataset.
// It reads history only; nothing is written to the repository.
func (b *builder) build(ctx context.Context, options buildOptions) (dataset, error) {
	if strings.TrimSpace(options.Version) == "" {
		return dataset{}, fmt.Errorf("a dataset version is required")
	}
	if options.MaxFixes < 1 {
		options.MaxFixes = defaultMaxFixes
	}
	header := datasetHeader{
		Schema: datasetSchemaVersion, Version: options.Version, Range: options.Range,
		SplitRule: splitRule, MaxFixes: options.MaxFixes, Filtered: map[string]int{}, Dropped: map[string]int{},
	}
	for _, filter := range []string{filterWhitespace, filterFormat, filterMove, filterPureRefactor} {
		header.Filtered[filter] = 0
	}
	if err := b.resolveRange(ctx, &header); err != nil {
		return dataset{}, err
	}
	fixes, truncated, err := b.findFixes(ctx, options)
	if err != nil {
		return dataset{}, err
	}
	header.FixesFound, header.Truncated = len(fixes), truncated
	labels := map[string]handLabel{}
	for _, label := range options.HandLabels {
		labels[label.FixCommit] = label
	}
	var cases []datasetCase
	for _, fix := range fixes {
		item, dropped, err := b.trace(ctx, fix, header.Filtered)
		if err != nil {
			return dataset{}, fmt.Errorf("trace fix %s: %w", fix.SHA, err)
		}
		if dropped != "" {
			header.Dropped[dropped]++
			continue
		}
		if label, ok := matchLabel(labels, fix.SHA); ok {
			label.FixCommit = ""
			item.HandLabel = &label
		}
		cases = append(cases, item)
	}
	set := dataset{Header: header, Cases: cases}
	set.Header.CaseCount, set.Header.Digest = len(cases), casesDigest(cases)
	return set, set.validate()
}

func matchLabel(labels map[string]handLabel, fix string) (handLabel, bool) {
	for prefix, label := range labels {
		if prefix != "" && strings.HasPrefix(fix, prefix) {
			return label, true
		}
	}
	return handLabel{}, false
}

func (b *builder) resolveRange(ctx context.Context, header *datasetHeader) error {
	from, to, isRange := strings.Cut(header.Range, "..")
	if !isRange {
		from, to = "", header.Range
	}
	if strings.TrimSpace(to) == "" {
		return fmt.Errorf("a commit range ending in a revision is required")
	}
	resolved, err := b.git.run(ctx, "rev-parse", "--verify", to+"^{commit}")
	if err != nil {
		return err
	}
	header.ToCommit = strings.TrimSpace(resolved)
	if from != "" {
		resolved, err := b.git.run(ctx, "rev-parse", "--verify", from+"^{commit}")
		if err != nil {
			return err
		}
		header.FromCommit = strings.TrimSpace(resolved)
	}
	return nil
}

// findFixes lists the range's first-parent, non-merge commits that are bug
// fixes, oldest first, stopping after MaxFixes.
func (b *builder) findFixes(ctx context.Context, options buildOptions) ([]fixCommit, bool, error) {
	out, err := b.git.run(ctx, "log", "--first-parent", "--no-merges", "--reverse",
		"--format=%H%x1f%P%x1f%s%x1f%b%x1e", options.Range)
	if err != nil {
		return nil, false, err
	}
	var fixes []fixCommit
	for _, record := range strings.Split(out, "\x1e") {
		fields := strings.Split(strings.TrimLeft(record, "\r\n"), "\x1f")
		if len(fields) < 4 || strings.TrimSpace(fields[1]) == "" {
			continue
		}
		fix := fixCommit{SHA: fields[0], Parent: strings.Fields(fields[1])[0], Subject: fields[2]}
		fix.Source, fix.Issues = classifyFix(fix.SHA, fields[2], fields[3], options)
		if fix.Source == "" {
			continue
		}
		if len(fixes) == options.MaxFixes {
			return fixes, true, nil
		}
		fixes = append(fixes, fix)
	}
	return fixes, false, nil
}

func classifyFix(sha, subject, body string, options buildOptions) (string, []int) {
	var bugIssues []int
	for _, match := range issueReference.FindAllStringSubmatch(subject+"\n"+body, -1) {
		number, err := strconv.Atoi(match[1])
		if err == nil && options.BugIssues[number] && !containsInt(bugIssues, number) {
			bugIssues = append(bugIssues, number)
		}
	}
	sort.Ints(bugIssues)
	for _, listed := range options.ListedFixes {
		if listed != "" && strings.HasPrefix(sha, listed) {
			return fixSourceListed, bugIssues
		}
	}
	if len(bugIssues) > 0 {
		return fixSourceBugIssue, bugIssues
	}
	if !options.NoConventional && conventionalFix.MatchString(subject) {
		return fixSourceConvention, nil
	}
	return "", nil
}

func containsInt(values []int, value int) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

// trace runs R-SZZ for one fix: filter its deleted lines, blame the rest in
// the fix's parent past pure-refactor commits, and take the commit holding
// the most traced lines as the culprit. It returns a drop reason instead of
// a case when no culprit can be named with confidence.
func (b *builder) trace(ctx context.Context, fix fixCommit, filtered map[string]int) (datasetCase, string, error) {
	diff, err := b.git.run(ctx, "diff", "-U0", "--no-color", "--no-ext-diff", "--no-renames", fix.Parent, fix.SHA)
	if err != nil {
		return datasetCase{}, "", err
	}
	files := parseUnifiedDiff(diff)
	removed := map[string]int{}
	lines := filterFixLines(files, removed)
	deleted := len(lines)
	for _, count := range removed {
		deleted += count
	}
	if deleted == 0 {
		return datasetCase{}, dropNoTraceableLines, nil
	}
	entries, err := b.blameFixLines(ctx, fix.Parent, lines, removed)
	for filter, count := range removed {
		filtered[filter] += count
	}
	if err != nil {
		return datasetCase{}, "", err
	}
	if len(entries) == 0 {
		return datasetCase{}, dropAllLinesFiltered, nil
	}
	candidates := rankCandidates(entries)
	top := candidates[0]
	if entriesBoundary(entries, top.Commit) {
		return datasetCase{}, dropRootCommit, nil
	}
	if top.Confidence < minCaseConfidence {
		return datasetCase{}, dropAmbiguous, nil
	}
	item := datasetCase{
		ID: "fix-" + fix.SHA[:12], Split: splitOf(top.Commit), FixCommit: fix.SHA, FixSubject: fix.Subject,
		FixSource: fix.Source, FixIssues: fix.Issues, IntroducingCommit: top.Commit, Confidence: top.Confidence,
		Candidates: candidates, BugLines: bugLines(entries, top.Commit), FixFiles: fixFiles(files), Filtered: nonZero(removed),
	}
	if len(candidates) > 1 {
		item.Flags = append(item.Flags, flagMultipleCandidates)
	}
	outOfDiff, err := b.outOfDiff(ctx, top.Commit, files)
	if err != nil {
		return datasetCase{}, "", err
	}
	if outOfDiff {
		item.OutOfDiff = true
		item.Flags = append(item.Flags, flagOutOfDiff)
	}
	return item, "", nil
}

// blameFixLines blames each file's lines in rev and traces any line that
// lands on a pure-refactor commit back through it (see traceRefactor). Every
// line whose first blame landed on a refactor is counted under
// filterPureRefactor; one still on a refactor after maxRefactorRounds is
// dropped.
func (b *builder) blameFixLines(ctx context.Context, rev string, lines []fixLine, removed map[string]int) ([]blameEntry, error) {
	byPath := map[string][]int{}
	var paths []string
	for _, line := range lines {
		if _, ok := byPath[line.Path]; !ok {
			paths = append(paths, line.Path)
		}
		byPath[line.Path] = append(byPath[line.Path], line.Line)
	}
	sort.Strings(paths)
	var traced []blameEntry
	for _, path := range paths {
		entries, err := b.blame(ctx, rev, path, byPath[path])
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			refactor, err := b.isPureRefactor(ctx, entry.Commit, entry.Boundary)
			if err != nil {
				return nil, err
			}
			if !refactor {
				traced = append(traced, entry)
				continue
			}
			removed[filterPureRefactor]++
			origin, ok, err := b.traceRefactor(ctx, entry)
			if err != nil {
				return nil, err
			}
			if ok {
				traced = append(traced, origin)
			}
		}
	}
	return traced, nil
}

func (b *builder) blame(ctx context.Context, rev, path string, lines []int) ([]blameEntry, error) {
	args := []string{"blame", "--line-porcelain", "-w", "-M", "-C"}
	for _, lineRange := range lineRanges(lines) {
		args = append(args, "-L", lineRange)
	}
	out, err := b.git.run(ctx, append(args, rev, "--", path)...)
	if err != nil {
		return nil, err
	}
	return parseLinePorcelain(out), nil
}

// traceRefactor follows a line blamed on a pure-refactor commit to where the
// refactor took it from, the deleted line with the same text (ignoring
// whitespace), and blames that line in the refactor's parent. Git's own
// --ignore-rev cannot follow reordered lines, which content matching can.
func (b *builder) traceRefactor(ctx context.Context, entry blameEntry) (blameEntry, bool, error) {
	for round := 0; round < maxRefactorRounds; round++ {
		path, line, ok := refactorOrigin(b.refactorFiles[entry.Commit], entry)
		if !ok {
			return blameEntry{}, false, nil
		}
		entries, err := b.blame(ctx, entry.Commit+"^", path, []int{line})
		if err != nil || len(entries) != 1 {
			return blameEntry{}, false, err
		}
		entry = entries[0]
		refactor, err := b.isPureRefactor(ctx, entry.Commit, entry.Boundary)
		if err != nil || !refactor {
			return entry, err == nil, err
		}
	}
	return blameEntry{}, false, nil
}

// refactorOrigin finds the line a refactor deleted to produce entry's line:
// same text ignoring whitespace, preferring the same file, then the nearest
// line number.
func refactorOrigin(files []diffFile, entry blameEntry) (string, int, bool) {
	text := normalized(entry.Text)
	if text == "" {
		return "", 0, false
	}
	path, line, best := "", 0, -1
	for _, file := range files {
		for _, hunk := range file.Hunks {
			for _, deleted := range hunk.Deleted {
				if normalized(deleted.Text) != text {
					continue
				}
				distance := deleted.Number - entry.Line
				if distance < 0 {
					distance = -distance
				}
				if file.OldPath != entry.Path {
					distance += 1 << 30
				}
				if best < 0 || distance < best {
					path, line, best = file.OldPath, deleted.Number, distance
				}
			}
		}
	}
	return path, line, best >= 0
}

func (b *builder) isPureRefactor(ctx context.Context, commit string, boundary bool) (bool, error) {
	if boundary {
		return false, nil
	}
	if refactor, ok := b.refactor[commit]; ok {
		return refactor, nil
	}
	diff, err := b.git.run(ctx, "diff", "-U0", "--no-color", "--no-ext-diff", "--no-renames", commit+"^", commit)
	if err != nil {
		return false, err
	}
	files := parseUnifiedDiff(diff)
	refactor := pureRefactorDiff(files)
	b.refactor[commit] = refactor
	if refactor {
		b.refactorFiles[commit] = files
	}
	return refactor, nil
}

// outOfDiff reports whether most of the fix's changed lines lie in files the
// introducing commit never touched: the bug's cause sat outside the diff a
// reviewer would have seen (for example a caller it failed to update).
func (b *builder) outOfDiff(ctx context.Context, introducing string, fix []diffFile) (bool, error) {
	touched, ok := b.changed[introducing]
	if !ok {
		out, err := b.git.run(ctx, "diff", "--name-only", "--no-renames", introducing+"^", introducing)
		if err != nil {
			return false, err
		}
		touched = map[string]bool{}
		for _, path := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
			if path = strings.TrimSpace(path); path != "" {
				touched[path] = true
			}
		}
		b.changed[introducing] = touched
	}
	outside, total := 0, 0
	for _, file := range fix {
		if isTestPath(file.OldPath) || isTestPath(file.NewPath) {
			continue
		}
		changed := 0
		for _, hunk := range file.Hunks {
			changed += len(hunk.Deleted) + len(hunk.Added)
		}
		total += changed
		if !touched[file.OldPath] && !touched[file.NewPath] {
			outside += changed
		}
	}
	return outside*2 > total, nil
}

// isTestPath reports whether a path holds tests. A fix's new regression test
// says nothing about where the bug's cause lay, so tests are left out of the
// out-of-diff measure.
func isTestPath(path string) bool {
	path = "/" + strings.ToLower(path)
	for _, marker := range []string{"_test.", ".test.", ".spec.", "/test/", "/tests/", "/testdata/", "/__tests__/"} {
		if strings.Contains(path, marker) {
			return true
		}
	}
	return false
}

func rankCandidates(entries []blameEntry) []candidate {
	counts := map[string]int{}
	for _, entry := range entries {
		counts[entry.Commit]++
	}
	candidates := make([]candidate, 0, len(counts))
	for commit, lines := range counts {
		candidates = append(candidates, candidate{Commit: commit, Lines: lines, Confidence: round4(float64(lines) / float64(len(entries)))})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Lines != candidates[j].Lines {
			return candidates[i].Lines > candidates[j].Lines
		}
		return candidates[i].Commit < candidates[j].Commit
	})
	return candidates
}

func entriesBoundary(entries []blameEntry, commit string) bool {
	for _, entry := range entries {
		if entry.Commit == commit && entry.Boundary {
			return true
		}
	}
	return false
}

func bugLines(entries []blameEntry, commit string) []fileLines {
	byPath := map[string][]int{}
	for _, entry := range entries {
		if entry.Commit == commit && !containsInt(byPath[entry.Path], entry.Line) {
			byPath[entry.Path] = append(byPath[entry.Path], entry.Line)
		}
	}
	result := make([]fileLines, 0, len(byPath))
	for path, lines := range byPath {
		sort.Ints(lines)
		result = append(result, fileLines{Path: path, Lines: lines})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func fixFiles(files []diffFile) []string {
	var paths []string
	for _, file := range files {
		for _, path := range uniquePaths(file.OldPath, file.NewPath) {
			if !containsString(paths, path) {
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

func nonZero(counts map[string]int) map[string]int {
	result := map[string]int{}
	for key, count := range counts {
		if count > 0 {
			result[key] = count
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func round4(value float64) float64 {
	return math.Round(value*1e4) / 1e4
}
