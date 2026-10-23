package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type diffLine struct {
	Number int
	Text   string
}

type diffHunk struct {
	Deleted []diffLine
	Added   []diffLine
}

// diffFile is one file of a unified diff. OldPath is empty for an added file
// and NewPath for a deleted one.
type diffFile struct {
	OldPath string
	NewPath string
	Hunks   []diffHunk
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parseUnifiedDiff reads `git diff` output. Hunk line counts decide where a
// hunk ends, so a deleted line that itself starts with "-- " is never taken
// for a file header.
func parseUnifiedDiff(text string) []diffFile {
	var files []diffFile
	var current *diffFile
	var hunk *diffHunk
	oldLine, newLine, oldLeft, newLeft := 0, 0, 0, 0
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if oldLeft > 0 || newLeft > 0 {
			switch {
			case strings.HasPrefix(line, "-"):
				hunk.Deleted = append(hunk.Deleted, diffLine{Number: oldLine, Text: line[1:]})
				oldLine, oldLeft = oldLine+1, oldLeft-1
			case strings.HasPrefix(line, "+"):
				hunk.Added = append(hunk.Added, diffLine{Number: newLine, Text: line[1:]})
				newLine, newLeft = newLine+1, newLeft-1
			case strings.HasPrefix(line, " "):
				oldLine, newLine, oldLeft, newLeft = oldLine+1, newLine+1, oldLeft-1, newLeft-1
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files = append(files, diffFile{})
			current, hunk = &files[len(files)-1], nil
		case current == nil:
		case strings.HasPrefix(line, "--- "):
			current.OldPath = diffPath(line[4:], "a/")
		case strings.HasPrefix(line, "+++ "):
			current.NewPath = diffPath(line[4:], "b/")
		case strings.HasPrefix(line, "@@ "):
			match := hunkHeader.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			oldLine, oldLeft = atoi(match[1]), countOf(match[2])
			newLine, newLeft = atoi(match[3]), countOf(match[4])
			current.Hunks = append(current.Hunks, diffHunk{})
			hunk = &current.Hunks[len(current.Hunks)-1]
		}
	}
	return files
}

func diffPath(raw, prefix string) string {
	raw = strings.TrimSuffix(raw, "\t")
	if raw == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(raw, `"`) {
		if unquoted, err := strconv.Unquote(raw); err == nil {
			raw = unquoted
		}
	}
	return strings.TrimPrefix(raw, prefix)
}

func atoi(value string) int {
	number, _ := strconv.Atoi(value)
	return number
}

// countOf is a hunk header's line count, which git omits when it is 1.
func countOf(value string) int {
	if value == "" {
		return 1
	}
	return atoi(value)
}

// normalized strips all whitespace, so lines differing only in formatting
// compare equal.
func normalized(text string) string {
	return strings.Join(strings.Fields(text), "")
}

// fixLine is a line, in the fix's parent, that the fix deleted or modified.
type fixLine struct {
	Path string
	Line int
}

// filterFixLines applies the line-level R-SZZ filters to a fix's diff and
// returns the deleted lines left to blame, counting what each filter
// removed. A deleted line re-added with only whitespace changed in the same
// hunk is a format change; one re-added anywhere else in the fix is a move.
func filterFixLines(files []diffFile, removed map[string]int) []fixLine {
	global := map[string]int{}
	for _, file := range files {
		for _, hunk := range file.Hunks {
			for _, added := range hunk.Added {
				global[normalized(added.Text)]++
			}
		}
	}
	type pending struct {
		path string
		line diffLine
	}
	var unmatched []pending
	for _, file := range files {
		if file.OldPath == "" {
			continue
		}
		for _, hunk := range file.Hunks {
			local := map[string]int{}
			for _, added := range hunk.Added {
				local[normalized(added.Text)]++
			}
			for _, deleted := range hunk.Deleted {
				key := normalized(deleted.Text)
				switch {
				case key == "":
					removed[filterWhitespace]++
				case local[key] > 0:
					local[key]--
					global[key]--
					removed[filterFormat]++
				default:
					unmatched = append(unmatched, pending{path: file.OldPath, line: deleted})
				}
			}
		}
	}
	var kept []fixLine
	for _, item := range unmatched {
		key := normalized(item.line.Text)
		if global[key] > 0 {
			global[key]--
			removed[filterMove]++
			continue
		}
		kept = append(kept, fixLine{Path: item.path, Line: item.line.Number})
	}
	return kept
}

// pureRefactorDiff reports whether a commit's diff only reformats or moves
// code: its non-blank deleted and added lines are the same multiset once
// whitespace is ignored.
func pureRefactorDiff(files []diffFile) bool {
	balance := map[string]int{}
	changed := false
	for _, file := range files {
		for _, hunk := range file.Hunks {
			for _, deleted := range hunk.Deleted {
				if key := normalized(deleted.Text); key != "" {
					balance[key]++
					changed = true
				}
			}
			for _, added := range hunk.Added {
				if key := normalized(added.Text); key != "" {
					balance[key]--
					changed = true
				}
			}
		}
	}
	for _, count := range balance {
		if count != 0 {
			return false
		}
	}
	return changed
}

func uniquePaths(paths ...string) []string {
	var unique []string
	for _, path := range paths {
		if path != "" && !containsString(unique, path) {
			unique = append(unique, path)
		}
	}
	return unique
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

// blameEntry is one fix line traced to the commit that last changed it, at
// its path and line number in that commit.
type blameEntry struct {
	Commit   string
	Path     string
	Line     int
	Boundary bool
	Text     string
}

var blameHeader = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64}) (\d+) (\d+)`)

// parseLinePorcelain reads `git blame --line-porcelain` output.
func parseLinePorcelain(text string) []blameEntry {
	var entries []blameEntry
	var current *blameEntry
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "\t") {
			if current != nil {
				current.Text = line[1:]
			}
			current = nil
			continue
		}
		if current == nil {
			if match := blameHeader.FindStringSubmatch(line); match != nil {
				entries = append(entries, blameEntry{Commit: match[1], Line: atoi(match[2])})
				current = &entries[len(entries)-1]
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "filename "):
			current.Path = strings.TrimPrefix(line, "filename ")
		case line == "boundary":
			current.Boundary = true
		}
	}
	return entries
}

// lineRanges compresses sorted line numbers into blame -L ranges.
func lineRanges(lines []int) []string {
	sorted := append([]int(nil), lines...)
	sort.Ints(sorted)
	var ranges []string
	for index := 0; index < len(sorted); {
		end := index
		for end+1 < len(sorted) && sorted[end+1] <= sorted[end]+1 {
			end++
		}
		ranges = append(ranges, strconv.Itoa(sorted[index])+","+strconv.Itoa(sorted[end]))
		index = end + 1
	}
	return ranges
}
