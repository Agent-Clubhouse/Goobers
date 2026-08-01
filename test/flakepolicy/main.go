// Command flakepolicy rejects anonymous flake skips and workflow retries.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const quarantineHelper = "internal/flake/quarantine.go"

var retryToken = regexp.MustCompile(`(?i)(retry|retries|rerun|re-run|wretry|max[-_]attempts?)`)

// retryIdiom catches the two idiomatic bash "retry until success" shapes —
// `<command> && break` (loop again only on failure, stop on success) and
// `<command> || continue` (loop again on failure) — regardless of whether
// the surrounding text uses any retry-flavored word. This closes the exact
// bypass a keyword-only check misses: `for n in 1 2 3; do make test &&
// break; done` contains none of retryToken's words.
var retryIdiom = regexp.MustCompile(`(?i)(&&\s*break\b|\|\|\s*continue\b)`)

var flakeTerms = []string{
	"flake",
	"flaky",
	"quarantin",
	"intermittent",
	"nondetermin",
	"sporadic",
	"timing-sensitive",
	"unstable test",
}

type violation struct {
	Path    string
	Line    int
	Message string
}

func main() {
	violations, err := checkRepository(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "flake policy:", err)
		os.Exit(1)
	}
	if len(violations) == 0 {
		return
	}
	for _, current := range violations {
		fmt.Fprintf(os.Stderr, "%s:%d: %s\n", current.Path, current.Line, current.Message)
	}
	os.Exit(1)
}

func checkRepository(root string) ([]violation, error) {
	var violations []violation
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			switch relative {
			case ".git", ".goobers", "bin", "portal/node_modules", "stress-results":
				return filepath.SkipDir
			default:
				return nil
			}
		}
		switch {
		case strings.HasSuffix(relative, ".go") && relative != quarantineHelper:
			found, err := checkGoFile(path, relative)
			if err != nil {
				return err
			}
			violations = append(violations, found...)
		case (strings.HasSuffix(relative, ".yml") || strings.HasSuffix(relative, ".yaml")) &&
			strings.HasPrefix(relative, ".github/workflows/"):
			found, err := checkWorkflow(path, relative)
			if err != nil {
				return err
			}
			violations = append(violations, found...)
		}
		return nil
	})
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Path == violations[j].Path {
			return violations[i].Line < violations[j].Line
		}
		return violations[i].Path < violations[j].Path
	})
	return violations, err
}

func checkGoFile(path, relative string) ([]violation, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, path, source, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", relative, err)
	}
	// Resolved ahead of the skip-call scan below so a skip reason passed by
	// variable — `reason := "flaky"; t.Skip(reason)` — is still checked
	// against flakeTerms even though the call expression's own source text
	// ("t.Skip(reason)") does not itself contain any flake word.
	stringLiterals := collectStringAssignments(parsed)
	var violations []violation
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Skip" && selector.Sel.Name != "Skipf" && selector.Sel.Name != "SkipNow" {
			return true
		}
		start := files.Position(call.Pos())
		end := files.Position(call.End())
		if start.Offset < 0 || end.Offset > len(source) || start.Offset >= end.Offset {
			return true
		}
		callText := strings.ToLower(string(source[start.Offset:end.Offset]))
		matched := containsFlakeTerm(callText)
		if !matched {
			for _, arg := range call.Args {
				ident, ok := arg.(*ast.Ident)
				if !ok {
					continue
				}
				if value, ok := stringLiterals[ident.Name]; ok && containsFlakeTerm(strings.ToLower(value)) {
					matched = true
					break
				}
			}
		}
		if matched {
			violations = append(violations, violation{
				Path:    relative,
				Line:    start.Line,
				Message: "raw flake skip is forbidden; use flake.Quarantine with an issue and expiry",
			})
		}
		return true
	})
	return violations, nil
}

func containsFlakeTerm(text string) bool {
	for _, term := range flakeTerms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

// collectStringAssignments maps identifier names to the string value of the
// last literal (or literal-concatenation) assignment found anywhere in the
// file. It is a best-effort, whole-file heuristic — not real scope or
// control-flow tracking — sufficient to catch a skip reason named through a
// local variable instead of written inline.
func collectStringAssignments(file *ast.File) map[string]string {
	values := make(map[string]string)
	record := func(name string, expr ast.Expr) {
		if value, ok := literalStringValue(expr); ok {
			values[name] = value
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch stmt := node.(type) {
		case *ast.AssignStmt:
			for index, lhs := range stmt.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || index >= len(stmt.Rhs) {
					continue
				}
				record(ident.Name, stmt.Rhs[index])
			}
		case *ast.ValueSpec:
			for index, name := range stmt.Names {
				if index >= len(stmt.Values) {
					continue
				}
				record(name.Name, stmt.Values[index])
			}
		}
		return true
	})
	return values
}

func literalStringValue(expr ast.Expr) (string, bool) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		unquoted, err := strconv.Unquote(value.Value)
		if err != nil {
			return "", false
		}
		return unquoted, true
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return "", false
		}
		left, ok := literalStringValue(value.X)
		if !ok {
			return "", false
		}
		right, ok := literalStringValue(value.Y)
		if !ok {
			return "", false
		}
		return left + right, true
	default:
		return "", false
	}
}

func checkWorkflow(path, relative string) (_ []violation, returnErr error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	var violations []violation
	scanner := bufio.NewScanner(file)
	// runBlockIndent tracks a `run: |`/`run: >` block scalar's own
	// indentation while -1 means "not currently inside one" — standard YAML
	// block-scalar scoping (content stays part of the block only while more
	// indented than the key that opened it).
	runBlockIndent := -1
	for line := 1; scanner.Scan(); line++ {
		raw := scanner.Text()
		text := strings.TrimSpace(raw)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		indent := leadingSpaces(raw)
		if runBlockIndent >= 0 && indent <= runBlockIndent {
			runBlockIndent = -1
		}
		// A step can open with "run:" directly or, in the common YAML
		// list-item shorthand, "- run:" (a step with no separate name:
		// field) — strip the marker so both forms are recognized the same
		// way.
		unmarked := strings.TrimPrefix(text, "- ")
		inRunContent := runBlockIndent >= 0 || strings.HasPrefix(unmarked, "run:")
		// retryToken is a free-text word match, safe against YAML structure
		// (uses:, retries:, max_attempts: are single-purpose lines) but not
		// against shell script content: legitimate network-flakiness retry
		// loops (`go mod download`, `curl --retry N`) say "retry" in an echo
		// message or a curl flag with nothing to do with masking a flaky
		// test. Inside run: content only the precise shell-idiom check
		// applies.
		matched := retryIdiom.MatchString(text)
		if !inRunContent {
			matched = matched || retryToken.MatchString(text)
		}
		if matched {
			violations = append(violations, violation{
				Path:    relative,
				Line:    line,
				Message: "automatic workflow retries are forbidden; every rerun must reference a flake issue",
			})
		}
		if strings.HasPrefix(unmarked, "run:") && runScalarBlock.MatchString(unmarked) {
			runBlockIndent = indent
		}
	}
	return violations, scanner.Err()
}

var runScalarBlock = regexp.MustCompile(`^run:\s*[|>][+-]?\s*$`)

func leadingSpaces(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}
