// Command flakepolicy rejects anonymous flake skips and workflow retries.
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const quarantineHelper = "internal/flake/quarantine.go"

var retryToken = regexp.MustCompile(`(?i)(retry|retries|rerun|re-run|wretry|max[-_]attempts?)`)

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
		for _, term := range flakeTerms {
			if strings.Contains(callText, term) {
				violations = append(violations, violation{
					Path:    relative,
					Line:    start.Line,
					Message: "raw flake skip is forbidden; use flake.Quarantine with an issue and expiry",
				})
				break
			}
		}
		return true
	})
	return violations, nil
}

func checkWorkflow(path, relative string) ([]violation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var violations []violation
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if retryToken.MatchString(text) {
			violations = append(violations, violation{
				Path:    relative,
				Line:    line,
				Message: "automatic workflow retries are forbidden; every rerun must reference a flake issue",
			})
		}
	}
	return violations, scanner.Err()
}
