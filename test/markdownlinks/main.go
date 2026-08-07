// Command markdownlinks validates local links and heading anchors in repository documentation.
package main

import (
	"bufio"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

var rootDocuments = []string{
	"README.md",
	"reference-workflows/README.md",
	"examples/README.md",
}

type violation struct {
	Path   string
	Line   int
	Target string
	Reason string
}

type link struct {
	line   int
	target string
}

type document struct {
	anchors map[string]bool
	links   []link
}

func main() {
	violations, err := checkRepository(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "markdown links:", err)
		os.Exit(1)
	}
	for _, current := range violations {
		fmt.Fprintf(os.Stderr, "%s:%d: %s: %s\n", current.Path, current.Line, current.Target, current.Reason)
	}
	if len(violations) != 0 {
		os.Exit(1)
	}
}

func checkRepository(root string) ([]violation, error) {
	paths, err := documentationPaths(root)
	if err != nil {
		return nil, err
	}

	documents := make(map[string]document, len(paths))
	for _, relative := range paths {
		parsed, err := parseDocument(filepath.Join(root, filepath.FromSlash(relative)), relative)
		if err != nil {
			return nil, err
		}
		documents[relative] = parsed
	}

	var violations []violation
	for _, relative := range paths {
		for _, current := range documents[relative].links {
			found, err := validateLink(root, relative, current, documents)
			if err != nil {
				return nil, err
			}
			if found != nil {
				violations = append(violations, *found)
			}
		}
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Path == violations[j].Path {
			if violations[i].Line == violations[j].Line {
				return violations[i].Target < violations[j].Target
			}
			return violations[i].Line < violations[j].Line
		}
		return violations[i].Path < violations[j].Path
	})
	return violations, nil
}

func documentationPaths(root string) ([]string, error) {
	var paths []string
	docsRoot := filepath.Join(root, "docs")
	err := filepath.WalkDir(docsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, relative := range rootDocuments {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return nil, fmt.Errorf("required documentation %s: %w", relative, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("required documentation %s is a directory", relative)
		}
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	return paths, nil
}

func parseDocument(path, relative string) (document, error) {
	file, err := os.Open(path)
	if err != nil {
		return document{}, err
	}
	defer file.Close()

	result := document{anchors: make(map[string]bool)}
	slugCounts := make(map[string]int)
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	inFence := false
	fenceMarker := ""
	previousLine := ""
	for scanner.Scan() {
		lineNumber++
		lineText := scanner.Text()
		trimmed := strings.TrimLeft(lineText, " \t")
		if marker := fenceStart(trimmed); marker != "" {
			if !inFence {
				inFence = true
				fenceMarker = marker
			} else if marker == fenceMarker {
				inFence = false
				fenceMarker = ""
			}
			continue
		}
		if inFence {
			previousLine = ""
			continue
		}

		if heading, ok := atxHeading(trimmed); ok {
			addHeadingAnchor(result.anchors, slugCounts, heading)
		} else if isSetextUnderline(trimmed) && strings.TrimSpace(previousLine) != "" {
			addHeadingAnchor(result.anchors, slugCounts, strings.TrimSpace(previousLine))
		}
		for _, anchor := range htmlAnchors(lineText) {
			result.anchors[anchor] = true
		}
		for _, target := range markdownTargets(lineText) {
			result.links = append(result.links, link{line: lineNumber, target: target})
		}
		previousLine = lineText
	}
	if err := scanner.Err(); err != nil {
		return document{}, fmt.Errorf("read %s: %w", relative, err)
	}
	return result, nil
}

func addHeadingAnchor(anchors map[string]bool, counts map[string]int, heading string) {
	slug := headingSlug(heading)
	if count := counts[slug]; count != 0 {
		anchors[fmt.Sprintf("%s-%d", slug, count)] = true
	} else {
		anchors[slug] = true
	}
	counts[slug]++
}

func fenceStart(line string) string {
	if strings.HasPrefix(line, "```") {
		return "```"
	}
	if strings.HasPrefix(line, "~~~") {
		return "~~~"
	}
	return ""
}

func atxHeading(line string) (string, bool) {
	hashes := 0
	for hashes < len(line) && line[hashes] == '#' {
		hashes++
	}
	if hashes == 0 || hashes > 6 || hashes == len(line) || !unicode.IsSpace(rune(line[hashes])) {
		return "", false
	}
	heading := strings.TrimSpace(line[hashes:])
	heading = strings.TrimSpace(strings.TrimRight(heading, "#"))
	return heading, true
}

func isSetextUnderline(line string) bool {
	line = strings.TrimSpace(line)
	if len(line) == 0 || line[0] != '=' && line[0] != '-' {
		return false
	}
	for _, current := range line {
		if current != rune(line[0]) {
			return false
		}
	}
	return true
}

func headingSlug(heading string) string {
	heading = html.UnescapeString(heading)
	var slug strings.Builder
	for _, current := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(current), unicode.IsMark(current), unicode.IsNumber(current),
			unicode.Is(unicode.Pc, current), current == '-':
			slug.WriteRune(current)
		case unicode.IsSpace(current):
			slug.WriteByte('-')
		}
	}
	return slug.String()
}

func markdownTargets(line string) []string {
	var targets []string
	for offset := 0; offset < len(line); {
		closeLabel := strings.Index(line[offset:], "](")
		if closeLabel < 0 {
			break
		}
		start := offset + closeLabel + 2
		target, end, ok := inlineTarget(line, start)
		if ok && target != "" {
			targets = append(targets, target)
		}
		if end <= start {
			offset = start
		} else {
			offset = end
		}
	}

	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "[") {
		if marker := strings.Index(trimmed, "]:"); marker > 1 {
			target := strings.TrimSpace(trimmed[marker+2:])
			if strings.HasPrefix(target, "<") {
				if end := strings.IndexByte(target, '>'); end > 1 {
					targets = append(targets, target[1:end])
				}
			} else if fields := strings.Fields(target); len(fields) != 0 {
				targets = append(targets, fields[0])
			}
		}
	}
	return targets
}

func inlineTarget(line string, start int) (string, int, bool) {
	for start < len(line) && unicode.IsSpace(rune(line[start])) {
		start++
	}
	if start >= len(line) {
		return "", start, false
	}
	if line[start] == '<' {
		end := strings.IndexByte(line[start+1:], '>')
		if end < 0 {
			return "", start, false
		}
		return line[start+1 : start+1+end], start + end + 2, true
	}

	depth := 0
	escaped := false
	for index := start; index < len(line); index++ {
		current := line[index]
		if escaped {
			escaped = false
			continue
		}
		if current == '\\' {
			escaped = true
			continue
		}
		switch current {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return line[start:index], index + 1, true
			}
			depth--
		case ' ', '\t':
			if depth == 0 {
				return line[start:index], index, true
			}
		}
	}
	return "", start, false
}

func htmlAnchors(line string) []string {
	var anchors []string
	lower := strings.ToLower(line)
	for _, attribute := range []string{" id=", " name="} {
		for offset := 0; offset < len(lower); {
			index := strings.Index(lower[offset:], attribute)
			if index < 0 {
				break
			}
			start := offset + index + len(attribute)
			if start < len(line) && (line[start] == '"' || line[start] == '\'') {
				quote := line[start]
				if end := strings.IndexByte(line[start+1:], quote); end >= 0 {
					anchors = append(anchors, line[start+1:start+1+end])
					offset = start + end + 2
					continue
				}
			}
			offset = start
		}
	}
	return anchors
}

func validateLink(root, source string, current link, documents map[string]document) (*violation, error) {
	raw := html.UnescapeString(strings.TrimSpace(current.target))
	if raw == "" || strings.HasPrefix(raw, "//") {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return &violation{Path: source, Line: current.line, Target: current.target, Reason: "invalid link"}, nil
	}
	if parsed.IsAbs() {
		return nil, nil
	}

	decodedPath, err := url.PathUnescape(parsed.Path)
	if err != nil {
		return &violation{Path: source, Line: current.line, Target: current.target, Reason: "invalid path escape"}, nil
	}
	targetPath := source
	if decodedPath != "" {
		if strings.HasPrefix(decodedPath, "/") {
			targetPath = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimPrefix(decodedPath, "/"))))
		} else {
			targetPath = filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(source)), filepath.FromSlash(decodedPath))))
		}
	}
	if targetPath == "." || targetPath == ".." || strings.HasPrefix(targetPath, "../") {
		return &violation{Path: source, Line: current.line, Target: current.target, Reason: "target escapes repository"}, nil
	}

	info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(targetPath)))
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return &violation{Path: source, Line: current.line, Target: current.target, Reason: "target does not exist"}, nil
		}
		return nil, statErr
	}
	if parsed.Fragment == "" {
		return nil, nil
	}
	if info.IsDir() {
		return &violation{Path: source, Line: current.line, Target: current.target, Reason: "cannot validate anchor on a directory"}, nil
	}

	targetDocument, ok := documents[targetPath]
	if !ok {
		if !strings.EqualFold(filepath.Ext(targetPath), ".md") {
			return &violation{Path: source, Line: current.line, Target: current.target, Reason: "anchor target is not Markdown"}, nil
		}
		targetDocument, err = parseDocument(filepath.Join(root, filepath.FromSlash(targetPath)), targetPath)
		if err != nil {
			return nil, err
		}
		documents[targetPath] = targetDocument
	}
	fragment, err := url.PathUnescape(parsed.Fragment)
	if err != nil {
		return &violation{Path: source, Line: current.line, Target: current.target, Reason: "invalid anchor escape"}, nil
	}
	if !targetDocument.anchors[fragment] {
		return &violation{Path: source, Line: current.line, Target: current.target, Reason: "anchor does not exist"}, nil
	}
	return nil, nil
}
