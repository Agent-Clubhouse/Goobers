// Command complexitygate enforces a cyclomatic-complexity ceiling on the
// repository's Go functions.
//
// Three tiers, all measured from the same sweep:
//
//   - hard cap (40): a function at or above the cap fails the gate unless its
//     exact path+symbol key is listed in test/complexitygate/baseline.txt, or
//     it carries the documented escape hatch. The baseline is keyed by
//     path+symbol rather than by a count, so moving or renaming a baselined
//     function does not hand a new function free headroom.
//   - soft ratchet (25): the number of functions at or above the ratchet may
//     never exceed the recorded `soft-budget`. Decomposition that lowers the
//     count prints the tighter number to record next.
//   - report-only (15): the count is printed as a trend signal and never fails.
//
// Escape hatch: a function that must exceed the hard cap without a baseline
// entry carries an inline `//complexitygate:allow <justification>` comment in
// its doc comment or on its `func` line. The justification is mandatory —
// a bare directive fails the gate.
//
// Unlike the coverage gate, nothing here is excluded by path: cmd/ mains carry
// a meaningful share of the repository's most complex functions, so exempting
// them would blind the gate to exactly where complexity accretes. The sweep
// covers production Go only — _test.go files and testdata fixtures are out of
// scope, since a long table-driven test is not the complexity this gate exists
// to hold down.
//
// Usage:
//
//	go run ./test/complexitygate [repository-root]
//	go run ./test/complexitygate -write [repository-root]   # re-pin the baseline
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	hardCap   = 40
	softCap   = 25
	reportCap = 15

	baselinePath = "test/complexitygate/baseline.txt"
	directive    = "//complexitygate:allow"
)

const baselineHeader = `# Cyclomatic-complexity baseline for ` + "`go run ./test/complexitygate`" + `.
#
# Entries are keyed by <path>:<symbol> so that moving or renaming a function
# drops its entry and re-subjects it to the hard cap; the trailing number is
# the complexity measured when the entry was pinned and is informational.
# soft-budget is the permitted number of functions at or above the soft
# ratchet; it may be lowered, never raised.
#
# Regenerate after an intentional change with:
#
#   go run ./test/complexitygate -write
#
# Prefer decomposing the function to adding an entry. A function that genuinely
# must stay complex can instead carry an inline escape hatch on its declaration:
#
#   //complexitygate:allow <why this function cannot be decomposed>
`

type function struct {
	path          string
	symbol        string
	line          int
	complexity    int
	allowed       bool
	justification string
}

func (f function) key() string {
	return f.path + ":" + f.symbol
}

type baseline struct {
	softBudget int
	entries    map[string]int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	write := false
	var positional []string
	for _, arg := range args {
		if arg == "-write" || arg == "--write" {
			write = true
			continue
		}
		positional = append(positional, arg)
	}
	if len(positional) > 1 {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/complexitygate [-write] [repository-root]")
		return 2
	}
	root := "."
	if len(positional) == 1 {
		root = positional[0]
	}

	functions, err := scan(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "complexity-gate: scan: %v\n", err)
		return 1
	}

	if write {
		if err := writeBaseline(filepath.Join(root, filepath.FromSlash(baselinePath)), functions); err != nil {
			_, _ = fmt.Fprintf(stderr, "complexity-gate: write baseline: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "complexity-gate: wrote %s\n", baselinePath)
		return 0
	}

	base, err := readBaseline(filepath.Join(root, filepath.FromSlash(baselinePath)))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "complexity-gate: read baseline: %v\n", err)
		return 1
	}

	violations, advisories := evaluate(functions, base)
	for _, advisory := range advisories {
		_, _ = fmt.Fprintln(stdout, advisory)
	}
	if len(violations) > 0 {
		for _, violation := range violations {
			_, _ = fmt.Fprintln(stderr, violation)
		}
		_, _ = fmt.Fprintf(stderr, "complexity-gate: decompose the function, or re-pin %s with `go run ./test/complexitygate -write`\n", baselinePath)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "complexity-gate: no function at or above cc %d outside the baseline\n", hardCap)
	return 0
}

func evaluate(functions []function, base baseline) (violations, advisories []string) {
	seen := make(map[string]bool)
	softCount, reportCount := 0, 0
	for _, current := range functions {
		if current.complexity >= reportCap {
			reportCount++
		}
		if current.complexity >= softCap {
			softCount++
		}
		if current.allowed && current.justification == "" {
			violations = append(violations, fmt.Sprintf(
				"%s:%d: %s requires a justification: %s <why this function cannot be decomposed>",
				current.path, current.line, directive, directive,
			))
			continue
		}
		if current.complexity < hardCap {
			continue
		}
		recorded, pinned := base.entries[current.key()]
		if pinned {
			seen[current.key()] = true
			if current.complexity > recorded {
				advisories = append(advisories, fmt.Sprintf(
					"complexity-gate: %s:%d: %s grew from cc %d to cc %d since it was pinned",
					current.path, current.line, current.symbol, recorded, current.complexity,
				))
			}
			continue
		}
		if current.allowed {
			advisories = append(advisories, fmt.Sprintf(
				"complexity-gate: %s:%d: %s allowed at cc %d: %s",
				current.path, current.line, current.symbol, current.complexity, current.justification,
			))
			continue
		}
		violations = append(violations, fmt.Sprintf(
			"%s:%d: %s has cyclomatic complexity %d, at or above the hard cap of %d",
			current.path, current.line, current.symbol, current.complexity, hardCap,
		))
	}

	for key := range base.entries {
		if !seen[key] {
			advisories = append(advisories, fmt.Sprintf(
				"complexity-gate: stale baseline entry (now below cc %d, or gone): %s",
				hardCap, key,
			))
		}
	}
	sort.Strings(violations)
	sort.Strings(advisories)

	switch {
	case softCount > base.softBudget:
		violations = append(violations, fmt.Sprintf(
			"complexity-gate: %d functions at or above cc %d, over the soft budget of %d",
			softCount, softCap, base.softBudget,
		))
	case softCount < base.softBudget:
		advisories = append(advisories, fmt.Sprintf(
			"complexity-gate: %d functions at or above cc %d; lower soft-budget to %d in %s",
			softCount, softCap, softCount, baselinePath,
		))
	}
	advisories = append(advisories, fmt.Sprintf(
		"complexity-gate: %d functions at or above cc %d (report-only)",
		reportCount, reportCap,
	))
	return violations, advisories
}

func scan(root string) ([]function, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	var functions []function
	err = filepath.WalkDir(absoluteRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != absoluteRoot && skippedDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(absoluteRoot, path)
		if err != nil {
			return err
		}
		fileFunctions, err := scanFile(filepath.ToSlash(relative), path)
		if err != nil {
			return err
		}
		functions = append(functions, fileFunctions...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(functions, func(i, j int) bool {
		if functions[i].path != functions[j].path {
			return functions[i].path < functions[j].path
		}
		return functions[i].line < functions[j].line
	})
	return functions, nil
}

func skippedDirectory(name string) bool {
	switch name {
	case ".git", ".goobers", "node_modules", "testdata", "vendor":
		return true
	default:
		return false
	}
}

func scanFile(relative, path string) ([]function, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", relative, err)
	}
	var functions []function
	for _, declaration := range file.Decls {
		declared, ok := declaration.(*ast.FuncDecl)
		if !ok || declared.Body == nil {
			continue
		}
		allowed, justification := escapeHatch(fileSet, file, declared)
		functions = append(functions, function{
			path:          relative,
			symbol:        symbolName(declared),
			line:          fileSet.Position(declared.Pos()).Line,
			complexity:    complexity(declared),
			allowed:       allowed,
			justification: justification,
		})
	}
	return functions, nil
}

// symbolName renders a method as (Receiver).Name so that same-named methods on
// different types stay distinct baseline keys.
func symbolName(declared *ast.FuncDecl) string {
	name := declared.Name.Name
	if declared.Recv == nil || len(declared.Recv.List) == 0 {
		return name
	}
	return "(" + receiverName(declared.Recv.List[0].Type) + ")." + name
}

func receiverName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return "*" + receiverName(typed.X)
	case *ast.IndexExpr:
		return receiverName(typed.X)
	case *ast.IndexListExpr:
		return receiverName(typed.X)
	case *ast.Ident:
		return typed.Name
	default:
		return "?"
	}
}

// complexity counts branch points the way gocyclo does: one for the function
// itself, plus one per if, for, range, non-default case, non-default select
// case, and per && or || operator.
func complexity(declared *ast.FuncDecl) int {
	total := 1
	ast.Inspect(declared.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			total++
		case *ast.CaseClause:
			if len(typed.List) > 0 {
				total++
			}
		case *ast.CommClause:
			if typed.Comm != nil {
				total++
			}
		case *ast.BinaryExpr:
			if typed.Op == token.LAND || typed.Op == token.LOR {
				total++
			}
		}
		return true
	})
	return total
}

// escapeHatch reads the directive from the declaration's doc comment or from a
// comment on the `func` line itself.
func escapeHatch(fileSet *token.FileSet, file *ast.File, declared *ast.FuncDecl) (bool, string) {
	declarationLine := fileSet.Position(declared.Pos()).Line
	var comments []*ast.Comment
	if declared.Doc != nil {
		comments = append(comments, declared.Doc.List...)
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if fileSet.Position(comment.Pos()).Line == declarationLine {
				comments = append(comments, comment)
			}
		}
	}
	for _, comment := range comments {
		text := "//" + strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
		if !strings.HasPrefix(text, directive) {
			continue
		}
		return true, strings.TrimSpace(strings.TrimPrefix(text, directive))
	}
	return false, ""
}

func readBaseline(path string) (baseline, error) {
	file, err := os.Open(path)
	if err != nil {
		return baseline{}, err
	}
	defer func() { _ = file.Close() }()
	return parseBaseline(file)
}

func parseBaseline(reader io.Reader) (baseline, error) {
	parsed := baseline{softBudget: -1, entries: make(map[string]int)}
	scanner := bufio.NewScanner(reader)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return baseline{}, fmt.Errorf("line %d: want `<path>:<symbol> <complexity>` or `soft-budget <count>`", lineNumber)
		}
		value, err := strconv.Atoi(fields[1])
		if err != nil {
			return baseline{}, fmt.Errorf("line %d: %q is not a number", lineNumber, fields[1])
		}
		if fields[0] == "soft-budget" {
			if parsed.softBudget >= 0 {
				return baseline{}, fmt.Errorf("line %d: duplicate soft-budget", lineNumber)
			}
			parsed.softBudget = value
			continue
		}
		if !strings.Contains(fields[0], ":") {
			return baseline{}, fmt.Errorf("line %d: key %q is not <path>:<symbol>", lineNumber, fields[0])
		}
		if _, duplicate := parsed.entries[fields[0]]; duplicate {
			return baseline{}, fmt.Errorf("line %d: duplicate entry %q", lineNumber, fields[0])
		}
		parsed.entries[fields[0]] = value
	}
	if err := scanner.Err(); err != nil {
		return baseline{}, err
	}
	if parsed.softBudget < 0 {
		return baseline{}, fmt.Errorf("missing `soft-budget <count>`")
	}
	return parsed, nil
}

func writeBaseline(path string, functions []function) error {
	return os.WriteFile(path, []byte(renderBaseline(functions)), 0o644)
}

func renderBaseline(functions []function) string {
	softCount := 0
	var lines []string
	for _, current := range functions {
		if current.complexity >= softCap {
			softCount++
		}
		if current.complexity < hardCap || current.allowed {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %d", current.key(), current.complexity))
	}
	sort.Strings(lines)
	var builder strings.Builder
	builder.WriteString(baselineHeader)
	builder.WriteString("\nsoft-budget ")
	builder.WriteString(strconv.Itoa(softCount))
	builder.WriteString("\n\n")
	for _, line := range lines {
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	return builder.String()
}
