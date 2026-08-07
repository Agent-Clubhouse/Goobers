// Command markdownlinks validates local links and heading anchors in repository documentation.
package main

import (
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
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
	source, err := os.ReadFile(path)
	if err != nil {
		return document{}, err
	}

	result := document{anchors: make(map[string]bool)}
	headingAnchors := make(map[string]bool)
	parsed := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(text.NewReader(source))
	lineStarts := sourceLineStarts(source)
	lastLinkOffset := 0
	err = ast.Walk(parsed, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch current := node.(type) {
		case *ast.Heading:
			addHeadingAnchor(result.anchors, headingAnchors, string(renderedText(current, source)))
		case *ast.Link:
			offset := nodeSourceOffset(current, source, lastLinkOffset)
			result.links = append(result.links, link{
				line:   sourceLine(lineStarts, offset),
				target: string(current.Destination),
			})
			lastLinkOffset = offset + 1
		case *ast.Image:
			offset := nodeSourceOffset(current, source, lastLinkOffset)
			result.links = append(result.links, link{
				line:   sourceLine(lineStarts, offset),
				target: string(current.Destination),
			})
			lastLinkOffset = offset + 1
		case *ast.HTMLBlock:
			block := current.Lines().Value(source)
			if current.HasClosure() {
				block = append(block, current.ClosureLine.Value(source)...)
			}
			addHTMLAnchors(result.anchors, block)
		case *ast.RawHTML:
			addHTMLAnchors(result.anchors, current.Segments.Value(source))
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return document{}, fmt.Errorf("parse %s: %w", relative, err)
	}
	return result, nil
}

func addHeadingAnchor(anchors, headingAnchors map[string]bool, heading string) {
	slug := headingSlug(heading)
	candidate := slug
	for suffix := 1; headingAnchors[candidate]; suffix++ {
		candidate = fmt.Sprintf("%s-%d", slug, suffix)
	}
	headingAnchors[candidate] = true
	anchors[candidate] = true
}

func renderedText(root ast.Node, source []byte) []byte {
	var result []byte
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch current := node.(type) {
		case *ast.Text:
			result = append(result, current.Value(source)...)
			if current.SoftLineBreak() || current.HardLineBreak() {
				result = append(result, ' ')
			}
		case *ast.String:
			result = append(result, current.Value...)
		}
		return ast.WalkContinue, nil
	})
	return result
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

func sourceLineStarts(source []byte) []int {
	starts := []int{0}
	for index, current := range source {
		if current == '\n' && index+1 < len(source) {
			starts = append(starts, index+1)
		}
	}
	return starts
}

func sourceLine(starts []int, offset int) int {
	return sort.Search(len(starts), func(index int) bool {
		return starts[index] > offset
	})
}

func nodeSourceOffset(root ast.Node, source []byte, after int) int {
	if offset, linkDepth, found := nodeContentOffset(root); found {
		for index := offset - 1; index >= after; index-- {
			if source[index] == '[' && !isEscaped(source, index) {
				linkDepth--
				if linkDepth == 0 {
					return index
				}
			}
		}
		return offset
	}

	parent := root.Parent()
	for parent != nil && parent.Lines().Len() == 0 {
		parent = parent.Parent()
	}
	if parent == nil {
		return after
	}
	lines := parent.Lines()
	start := max(lines.At(0).Start, after)
	end := lines.At(lines.Len() - 1).Stop
	for index := start; index+1 < end; index++ {
		if source[index] != '[' || source[index+1] != ']' || isEscaped(source, index) {
			continue
		}
		next := index + 2
		if next < end && (source[next] == '(' || source[next] == '[') {
			return index
		}
	}
	return start
}

func nodeContentOffset(node ast.Node) (int, int, bool) {
	linkDepth := 0
	switch node.(type) {
	case *ast.Link, *ast.Image:
		linkDepth = 1
	}

	switch current := node.(type) {
	case *ast.Text:
		return current.Segment.Start, linkDepth, true
	case *ast.RawHTML:
		if current.Segments.Len() != 0 {
			return current.Segments.At(0).Start, linkDepth, true
		}
	}

	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if offset, childDepth, found := nodeContentOffset(child); found {
			return offset, linkDepth + childDepth, true
		}
	}
	return 0, 0, false
}

func isEscaped(source []byte, offset int) bool {
	backslashes := 0
	for offset > 0 && source[offset-1] == '\\' {
		backslashes++
		offset--
	}
	return backslashes%2 != 0
}

func addHTMLAnchors(anchors map[string]bool, source []byte) {
	line := string(source)
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
					anchors[line[start+1:start+1+end]] = true
					offset = start + end + 2
					continue
				}
			}
			offset = start
		}
	}
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

	targetPath := source
	if parsed.Path != "" {
		if strings.HasPrefix(parsed.Path, "/") {
			targetPath = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimPrefix(parsed.Path, "/"))))
		} else {
			targetPath = filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(source)), filepath.FromSlash(parsed.Path))))
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
	if !targetDocument.anchors[parsed.Fragment] {
		return &violation{Path: source, Line: current.line, Target: current.target, Reason: "anchor does not exist"}, nil
	}
	return nil, nil
}
