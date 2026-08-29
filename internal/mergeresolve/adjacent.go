// Package mergeresolve mechanically resolves the narrow, provably safe class
// of integration conflicts that ordinary concurrency between two branches
// produces: both sides inserted one distinct entry into the same existing
// line-oriented list (a manifest script line, a YAML sequence, a Go slice
// literal). Such a conflict is not a code defect, so routing it to an
// implementation repass can only re-derive the identical diff and manufacture
// an escalation (#3096).
//
// The algorithm and the git plumbing live here — rather than in either
// consumer — because both the pr-remediation rebase (`goobers rebase-pr`) and
// the implementation workflow's pre-CI base synchronization
// (internal/worktree's syncBase merge) must apply exactly the same safety
// rules. Anything this package cannot resolve provably safely stays a
// conflict for its caller to report.
package mergeresolve

import (
	"bytes"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/yaml"
)

// MergeAdjacentLineInsertions merges the case where ancestor gained exactly
// one distinct new line on each side at the same unambiguous position inside
// the same line-oriented list, returning the merged content and true. Every
// other shape returns false — including two sides editing the same line,
// which is a genuine content conflict a human or an agent must resolve.
func MergeAdjacentLineInsertions(path string, ancestor, upstream, incoming []byte) ([]byte, bool) {
	if len(ancestor) == 0 ||
		bytes.IndexByte(ancestor, 0) >= 0 ||
		bytes.IndexByte(upstream, 0) >= 0 ||
		bytes.IndexByte(incoming, 0) >= 0 ||
		!utf8.Valid(ancestor) ||
		!utf8.Valid(upstream) ||
		!utf8.Valid(incoming) {
		return nil, false
	}

	ancestorLines := splitFileLines(ancestor)
	upstreamLines := splitFileLines(upstream)
	incomingLines := splitFileLines(incoming)
	upstreamAt, upstreamLine, ok := singleInsertedLine(ancestorLines, upstreamLines)
	if !ok {
		return nil, false
	}
	incomingAt, incomingLine, ok := singleInsertedLine(ancestorLines, incomingLines)
	if !ok || upstreamAt != incomingAt ||
		!strings.HasSuffix(upstreamLine, "\n") ||
		!strings.HasSuffix(incomingLine, "\n") ||
		strings.TrimSpace(upstreamLine) == "" ||
		strings.TrimSpace(upstreamLine) == strings.TrimSpace(incomingLine) ||
		leadingWhitespace(upstreamLine) != leadingWhitespace(incomingLine) ||
		!hasVerifiedMarkerListSyntax(path, ancestor, upstream, incoming, upstreamLine) ||
		!sameAdjacentList(ancestorLines, upstreamAt, upstreamLine, incomingLine) {
		return nil, false
	}

	merged := make([]string, 0, len(ancestorLines)+2)
	merged = append(merged, ancestorLines[:upstreamAt]...)
	merged = append(merged, upstreamLine, incomingLine)
	merged = append(merged, ancestorLines[upstreamAt:]...)
	return []byte(strings.Join(merged, "")), true
}

func hasVerifiedMarkerListSyntax(path string, ancestor, upstream, incoming []byte, insertedLine string) bool {
	kind := listEntryKind(insertedLine)
	if kind == "" {
		return false
	}
	if strings.HasPrefix(kind, "quoted ") {
		return true
	}
	if kind != "- " {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return false
	}
	for _, data := range [][]byte{ancestor, upstream, incoming} {
		var document any
		if err := yaml.Unmarshal(data, &document); err != nil {
			return false
		}
	}
	return true
}

func splitFileLines(data []byte) []string {
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func singleInsertedLine(ancestor, side []string) (int, string, bool) {
	if len(side) != len(ancestor)+1 {
		return 0, "", false
	}

	prefix := 0
	for prefix < len(ancestor) && ancestor[prefix] == side[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(ancestor) &&
		ancestor[len(ancestor)-1-suffix] == side[len(side)-1-suffix] {
		suffix++
	}
	insertAt := len(ancestor) - suffix
	if insertAt != prefix {
		return 0, "", false
	}
	return insertAt, side[insertAt], true
}

func leadingWhitespace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func sameAdjacentList(ancestor []string, insertAt int, upstream, incoming string) bool {
	kind := listEntryKind(upstream)
	if kind == "" || listEntryKind(incoming) != kind {
		return false
	}
	indent := leadingWhitespace(upstream)
	if strings.HasPrefix(kind, "quoted ") {
		if !hasQuotedListContainer(ancestor, insertAt, indent) {
			return false
		}
	} else if !hasMarkerListContainer(ancestor, insertAt, indent) {
		return false
	}
	for _, neighbor := range []int{insertAt - 1, insertAt} {
		if neighbor >= 0 && neighbor < len(ancestor) &&
			leadingWhitespace(ancestor[neighbor]) == indent &&
			listEntryKind(ancestor[neighbor]) == kind {
			return true
		}
	}
	return false
}

func hasMarkerListContainer(ancestor []string, insertAt int, entryIndent string) bool {
	if entryIndent == "" {
		return false
	}
	for i := insertAt - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(ancestor[i])
		if trimmed == "" {
			continue
		}
		indent := leadingWhitespace(ancestor[i])
		if len(indent) >= len(entryIndent) {
			continue
		}
		return strings.HasPrefix(entryIndent, indent) && strings.HasSuffix(trimmed, ":")
	}
	return false
}

func hasQuotedListContainer(ancestor []string, insertAt int, entryIndent string) bool {
	openerIndent := ""
	foundOpener := false
	for i := insertAt - 1; i >= 0; i-- {
		indent := leadingWhitespace(ancestor[i])
		if len(indent) >= len(entryIndent) || strings.TrimSpace(ancestor[i]) == "" {
			continue
		}
		if !strings.HasSuffix(strings.TrimSpace(ancestor[i]), "[") {
			return false
		}
		openerIndent = indent
		foundOpener = true
		break
	}
	if !foundOpener {
		return false
	}

	for i := insertAt; i < len(ancestor); i++ {
		indent := leadingWhitespace(ancestor[i])
		if len(indent) >= len(entryIndent) || strings.TrimSpace(ancestor[i]) == "" {
			continue
		}
		trimmed := strings.TrimSpace(ancestor[i])
		return indent == openerIndent && (trimmed == "]" || trimmed == "],")
	}
	return false
}

func listEntryKind(line string) string {
	line = strings.TrimSpace(line)
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, marker) {
			return marker
		}
	}
	for i := 0; i < len(line); i++ {
		if line[i] < '0' || line[i] > '9' {
			if i > 0 && len(line) > i+1 &&
				(line[i] == '.' || line[i] == ')') && line[i+1] == ' ' {
				return "ordered"
			}
			break
		}
	}
	if len(line) >= 3 && line[len(line)-1] == ',' {
		quote := line[0]
		switch quote {
		case '"', '\'', '`':
			for i := 1; i < len(line); i++ {
				if quote != '`' && line[i] == '\\' {
					i++
					continue
				}
				if line[i] == quote {
					if i == len(line)-2 {
						return "quoted " + string(quote)
					}
					return ""
				}
			}
		}
	}
	return ""
}
