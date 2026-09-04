// Command complexitygate keeps cyclomatic complexity from re-accreting after
// it has been paid down by hand (#4231).
//
// It parses every Go file in the tree, scores each function the way gocyclo
// does (1 + branch points), and enforces three tiers:
//
//	hard cap (default 40)  a function at or above the cap must be listed in
//	                       test/complexitygate/baseline.txt at no more than
//	                       its recorded score. A new offender, or a baselined
//	                       one that grew, fails the gate.
//	ratchet (default 25)   the number of functions at or above the ratchet
//	                       must not exceed the budget recorded in the
//	                       baseline. Dropping below it prints a note that the
//	                       budget can be tightened.
//	report (default 15)    counted and printed only; never fails.
//
// The baseline is keyed by file path plus symbol, so moving a function to
// another file does not hand it fresh headroom: the moved copy is an unknown
// key and trips the hard cap. Refresh the file with `make complexity-update`
// after a deliberate decomposition.
//
// Escape hatch: a function may carry an inline
//
//	//complexitygate:allow <justification>
//
// comment in its doc comment or body. The justification text is mandatory —
// a bare directive fails the gate — and an allowed function is exempt from
// the hard cap while still counting toward the ratchet budget.
//
// Unlike test/coveragegate this gate does NOT exclude cmd/: command mains are
// where complexity has grown fastest.
package main

import (
	"bufio"
	"flag"
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
	defaultBaselinePath = "test/complexitygate/baseline.txt"
	defaultHardCap      = 40
	defaultRatchet      = 25
	defaultReport       = 15

	allowDirective  = "//complexitygate:allow"
	budgetDirective = "!ratchet-budget"
)

// skippedDirectories are trees that hold no first-party Go code worth scoring.
var skippedDirectories = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
	"bin":          true,
}

type function struct {
	Path       string
	Symbol     string
	Complexity int
	Line       int
	Allowed    bool
	AllowBlank bool
}

type baseline struct {
	Entries       map[string]int
	RatchetBudget int
}

type thresholds struct {
	hardCap int
	ratchet int
	report  int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("complexitygate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "repository root to scan")
	baselinePath := flags.String("baseline", defaultBaselinePath, "baseline file, relative to -root")
	update := flags.Bool("update", false, "rewrite the baseline from the current tree")
	hardCap := flags.Int("hard", defaultHardCap, "complexity at or above which a function must be baselined")
	ratchet := flags.Int("ratchet", defaultRatchet, "complexity counted against the baseline's budget")
	report := flags.Int("report", defaultReport, "complexity counted for the report-only tier")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	limits := thresholds{hardCap: *hardCap, ratchet: *ratchet, report: *report}
	if limits.hardCap < 1 || limits.ratchet < 1 || limits.report < 1 {
		_, _ = fmt.Fprintln(stderr, "complexitygate: thresholds must be positive")
		return 2
	}

	functions, err := scanTree(*root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "complexitygate: scan: %v\n", err)
		return 1
	}

	resolved := *baselinePath
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(*root, resolved)
	}

	if *update {
		if err := writeBaseline(resolved, functions, limits); err != nil {
			_, _ = fmt.Fprintf(stderr, "complexitygate: write baseline: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "complexitygate: wrote %s\n", *baselinePath)
		return 0
	}

	base, err := readBaseline(resolved)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "complexitygate: read baseline: %v\n", err)
		return 1
	}

	problems, notes := evaluate(functions, base, limits)
	for _, note := range notes {
		_, _ = fmt.Fprintln(stdout, note)
	}
	if len(problems) > 0 {
		for _, problem := range problems {
			_, _ = fmt.Fprintln(stderr, problem)
		}
		_, _ = fmt.Fprintf(stderr,
			"complexitygate: decompose the function, or record a deliberate exception with `%s <why>`; `make complexity-update` re-pins the baseline after a decomposition\n",
			allowDirective)
		return 1
	}

	_, _ = fmt.Fprintf(stdout,
		"complexitygate: %d functions >= %d (report-only), %d >= %d (budget %d), %d >= %d (baselined)\n",
		countAtLeast(functions, limits.report), limits.report,
		countAtLeast(functions, limits.ratchet), limits.ratchet, base.RatchetBudget,
		countAtLeast(functions, limits.hardCap), limits.hardCap,
	)
	return 0
}

func scanTree(root string) ([]function, error) {
	var functions []function
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && skippedDirectories[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		// Test files are excluded: a table-driven test is branchy by
		// construction, and its complexity is not the production complexity
		// this gate exists to hold down.
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		parsed, err := scanFile(filepath.ToSlash(relative), source)
		if err != nil {
			return err
		}
		functions = append(functions, parsed...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortFunctions(functions)
	return functions, nil
}

func scanFile(path string, source []byte) ([]function, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, source, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var functions []function
	for _, declaration := range file.Decls {
		declared, ok := declaration.(*ast.FuncDecl)
		if !ok || declared.Body == nil {
			continue
		}
		allowed, blank := allowance(file, declared)
		functions = append(functions, function{
			Path:       path,
			Symbol:     symbolName(declared),
			Complexity: complexity(declared),
			Line:       fileSet.Position(declared.Pos()).Line,
			Allowed:    allowed,
			AllowBlank: blank,
		})
	}
	return functions, nil
}

// allowance reports whether the declaration carries an escape-hatch directive
// and whether that directive omitted its mandatory justification.
func allowance(file *ast.File, declared *ast.FuncDecl) (allowed, blank bool) {
	start := declared.Pos()
	if declared.Doc != nil {
		start = declared.Doc.Pos()
	}
	end := declared.End()
	for _, group := range file.Comments {
		if group.Pos() < start || group.End() > end {
			continue
		}
		for _, comment := range group.List {
			text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(comment.Text), "// "))
			if !strings.HasPrefix(text, allowDirective) {
				continue
			}
			allowed = true
			if strings.TrimSpace(strings.TrimPrefix(text, allowDirective)) == "" {
				blank = true
			}
		}
	}
	return allowed, blank
}

func symbolName(declared *ast.FuncDecl) string {
	if declared.Recv == nil || len(declared.Recv.List) == 0 {
		return declared.Name.Name
	}
	return "(" + receiverName(declared.Recv.List[0].Type) + ")." + declared.Name.Name
}

func receiverName(expression ast.Expr) string {
	switch typed := expression.(type) {
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

// complexity scores a declaration the way gocyclo does: one, plus one for
// every branch point in the body.
func complexity(declared *ast.FuncDecl) int {
	score := 1
	ast.Inspect(declared.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			score++
		case *ast.CaseClause:
			if len(typed.List) > 0 {
				score++
			}
		case *ast.CommClause:
			if typed.Comm != nil {
				score++
			}
		case *ast.BinaryExpr:
			if typed.Op == token.LAND || typed.Op == token.LOR {
				score++
			}
		}
		return true
	})
	return score
}

func key(path, symbol string) string {
	return path + "\t" + symbol
}

func sortFunctions(functions []function) {
	sort.Slice(functions, func(i, j int) bool {
		if functions[i].Path != functions[j].Path {
			return functions[i].Path < functions[j].Path
		}
		return functions[i].Symbol < functions[j].Symbol
	})
}

func countAtLeast(functions []function, threshold int) int {
	count := 0
	for _, current := range functions {
		if current.Complexity >= threshold {
			count++
		}
	}
	return count
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
	result := baseline{Entries: make(map[string]int), RatchetBudget: -1}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimRight(scanner.Text(), " \t\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, budgetDirective) {
			raw := strings.TrimSpace(strings.TrimPrefix(line, budgetDirective))
			budget, err := strconv.Atoi(raw)
			if err != nil || budget < 0 {
				return baseline{}, fmt.Errorf("line %d: %s wants a non-negative integer, got %q", lineNumber, budgetDirective, raw)
			}
			result.RatchetBudget = budget
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return baseline{}, fmt.Errorf("line %d: want <path>\\t<symbol>\\t<complexity>", lineNumber)
		}
		score, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil || score < 1 {
			return baseline{}, fmt.Errorf("line %d: complexity %q is not a positive integer", lineNumber, fields[2])
		}
		entryKey := key(fields[0], fields[1])
		if _, duplicate := result.Entries[entryKey]; duplicate {
			return baseline{}, fmt.Errorf("line %d: duplicate entry %s %s", lineNumber, fields[0], fields[1])
		}
		result.Entries[entryKey] = score
	}
	if err := scanner.Err(); err != nil {
		return baseline{}, err
	}
	if result.RatchetBudget < 0 {
		return baseline{}, fmt.Errorf("missing %s directive", budgetDirective)
	}
	return result, nil
}

func evaluate(functions []function, base baseline, limits thresholds) (problems, notes []string) {
	seen := make(map[string]bool)
	for _, current := range functions {
		if current.AllowBlank {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: %s: %s needs a justification after the directive",
				current.Path, current.Line, current.Symbol, allowDirective,
			))
		}
		if current.Complexity < limits.hardCap {
			continue
		}
		entryKey := key(current.Path, current.Symbol)
		seen[entryKey] = true
		if current.Allowed {
			continue
		}
		recorded, baselined := base.Entries[entryKey]
		switch {
		case !baselined:
			problems = append(problems, fmt.Sprintf(
				"%s:%d: %s: cyclomatic complexity %d is at or above the hard cap of %d and is not in the baseline",
				current.Path, current.Line, current.Symbol, current.Complexity, limits.hardCap,
			))
		case current.Complexity > recorded:
			problems = append(problems, fmt.Sprintf(
				"%s:%d: %s: cyclomatic complexity grew from the baselined %d to %d",
				current.Path, current.Line, current.Symbol, recorded, current.Complexity,
			))
		}
	}

	for entryKey := range base.Entries {
		if seen[entryKey] {
			continue
		}
		path, symbol, _ := strings.Cut(entryKey, "\t")
		notes = append(notes, fmt.Sprintf(
			"complexitygate: stale baseline entry %s %s is now below the cap; drop it with `make complexity-update`",
			path, symbol,
		))
	}

	ratchetCount := countAtLeast(functions, limits.ratchet)
	switch {
	case ratchetCount > base.RatchetBudget:
		problems = append(problems, fmt.Sprintf(
			"complexitygate: %d functions are at or above the ratchet of %d, over the budget of %d",
			ratchetCount, limits.ratchet, base.RatchetBudget,
		))
	case ratchetCount < base.RatchetBudget:
		notes = append(notes, fmt.Sprintf(
			"complexitygate: %d functions are at or above the ratchet of %d, under the budget of %d; tighten it with `make complexity-update`",
			ratchetCount, limits.ratchet, base.RatchetBudget,
		))
	}
	sort.Strings(notes)
	return problems, notes
}

func writeBaseline(path string, functions []function, limits thresholds) error {
	var builder strings.Builder
	builder.WriteString("# Cyclomatic-complexity baseline for test/complexitygate (#4231).\n")
	builder.WriteString("# Generated by `make complexity-update`; do not hand-edit the scores.\n")
	builder.WriteString("#\n")
	fmt.Fprintf(&builder, "# Entries are every function at or above the hard cap of %d, keyed by\n", limits.hardCap)
	builder.WriteString("# <path>\\t<symbol>\\t<complexity>. The key is path+symbol, so moving a\n")
	builder.WriteString("# function to another file does not create headroom.\n")
	fmt.Fprintf(&builder, "# %s is how many functions may sit at or above %d.\n", budgetDirective, limits.ratchet)
	fmt.Fprintf(&builder, "%s %d\n", budgetDirective, countAtLeast(functions, limits.ratchet))
	for _, current := range functions {
		if current.Complexity < limits.hardCap || current.Allowed {
			continue
		}
		fmt.Fprintf(&builder, "%s\t%s\t%d\n", current.Path, current.Symbol, current.Complexity)
	}
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}
