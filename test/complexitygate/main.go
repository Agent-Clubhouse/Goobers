// Command complexitygate keeps cyclomatic complexity from re-accreting after
// it has been paid down by hand (#4231).
//
// It parses every Go file in the tree, scores each function the way gocyclo
// does (1 + branch points), measures declaration length, and enforces four tiers:
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
//	body length (default   functions at or above 200 lines must be in the
//	200 lines)             length baseline at no more than their recorded size.
//	                       New oversized functions cannot be baselined.
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
// a bare directive fails the gate — and an allowed function without a baseline
// entry is exempt from the hard cap while still counting toward the ratchet
// budget. Once an allowed function is baselined, its recorded score remains a
// ceiling and the ordinary growth and stale-entry checks apply.
// This escape hatch never exempts a function from the body-length tier.
//
// Unlike test/coveragegate this gate does NOT exclude cmd/: command mains are
// where complexity has grown fastest.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
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
	defaultBodyLength   = 200

	allowDirective               = "//complexitygate:allow"
	budgetDirective              = "!ratchet-budget"
	entryJustificationDirective  = "!entry-justification"
	budgetJustificationDirective = "!ratchet-budget-justification"
	bodyLengthCapDirective       = "!body-length-cap"
	bodyLengthDirective          = "!body-length"
	bodyLengthJustification      = "!body-length-justification"
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
	BodyLines  int
	Line       int
	Allowed    bool
	AllowBlank bool
}

type baseline struct {
	Entries              map[string]int
	EntryJustifications  map[string]justification
	RatchetBudget        int
	RatchetJustification *justification
	BodyLengths          map[string]int
	BodyJustifications   map[string]justification
	BodyLengthCap        int
}

type justification struct {
	Target int
	Reason string
}

type thresholds struct {
	hardCap int
	ratchet int
	report  int
	body    int
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
	bodyLength := flags.Int("body-length", defaultBodyLength, "function body length at or above which an existing baseline entry is required")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	limits := thresholds{hardCap: *hardCap, ratchet: *ratchet, report: *report, body: *bodyLength}
	if limits.hardCap < 1 || limits.ratchet < 1 || limits.report < 1 || limits.body < 1 {
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
		var candidate *baseline
		parsed, err := readBaseline(resolved)
		switch {
		case err == nil:
			candidate = &parsed
		case !errors.Is(err, os.ErrNotExist):
			_, _ = fmt.Fprintf(stderr, "complexitygate: read baseline for update: %v\n", err)
			return 1
		}
		previous, err := readCommittedBaseline(*root, resolved)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "complexitygate: read committed baseline for update: %v\n", err)
			return 1
		}
		if err := writeBaseline(resolved, functions, limits, previous, candidate); err != nil {
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
	if base.BodyLengthCap < 0 {
		_, _ = fmt.Fprintf(stderr, "complexitygate: read baseline: missing %s directive\n", bodyLengthCapDirective)
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
			"complexitygate: decompose the function; CC-only generated exceptions may use `%s <why>`, while the body-length cap has no exemption; `make complexity-update` records decreases and justified growth of existing entries\n",
			allowDirective)
		return 1
	}

	_, _ = fmt.Fprintf(stdout,
		"complexitygate: %d functions >= %d (report-only), %d >= %d (budget %d), %d >= %d (baselined), %d functions >= %d body lines (%d baselined)\n",
		countAtLeast(functions, limits.report), limits.report,
		countAtLeast(functions, limits.ratchet), limits.ratchet, base.RatchetBudget,
		countAtLeast(functions, limits.hardCap), limits.hardCap,
		countBodyLengthAtLeast(functions, limits.body), limits.body, len(base.BodyLengths),
	)
	return 0
}

func readCommittedBaseline(root, path string) (*baseline, error) {
	prefixOutput, err := exec.Command("git", "-C", root, "rev-parse", "--show-prefix").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("locate repository: %w: %s", err, strings.TrimSpace(string(prefixOutput)))
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve baseline path: %w", err)
	}
	relativePath, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("baseline %s is outside scan root %s", path, root)
	}

	if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "HEAD").Run(); err != nil {
		return nil, nil
	}
	repositoryPath := filepath.ToSlash(filepath.Join(strings.TrimSpace(string(prefixOutput)), relativePath))
	object := "HEAD:" + repositoryPath
	if err := exec.Command("git", "-C", root, "cat-file", "-e", object).Run(); err != nil {
		return nil, nil
	}
	content, err := exec.Command("git", "-C", root, "show", object).Output()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", object, err)
	}
	parsed, err := parseBaseline(bytes.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", object, err)
	}
	return &parsed, nil
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
		start := fileSet.PositionFor(declared.Pos(), false)
		end := fileSet.PositionFor(declared.End(), false)
		functions = append(functions, function{
			Path:       path,
			Symbol:     symbolName(declared),
			Complexity: complexity(declared),
			BodyLines:  end.Line - start.Line + 1,
			Line:       start.Line,
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

func countBodyLengthAtLeast(functions []function, threshold int) int {
	count := 0
	for _, current := range functions {
		if current.BodyLines >= threshold {
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
	result := baseline{
		Entries:             make(map[string]int),
		EntryJustifications: make(map[string]justification),
		RatchetBudget:       -1,
		BodyLengths:         make(map[string]int),
		BodyJustifications:  make(map[string]justification),
		BodyLengthCap:       -1,
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimRight(scanner.Text(), " \t\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		handled, err := parseJustificationLine(line, lineNumber, &result)
		if err != nil {
			return baseline{}, err
		}
		if handled {
			continue
		}
		handled, err = parseBodyLengthLine(line, lineNumber, &result)
		if err != nil {
			return baseline{}, err
		}
		if handled {
			continue
		}
		if strings.HasPrefix(line, budgetDirective+" ") {
			if result.RatchetBudget >= 0 {
				return baseline{}, fmt.Errorf("line %d: duplicate %s", lineNumber, budgetDirective)
			}
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

func parseBodyLengthLine(line string, lineNumber int, result *baseline) (bool, error) {
	if strings.HasPrefix(line, bodyLengthCapDirective+" ") {
		if result.BodyLengthCap >= 0 {
			return true, fmt.Errorf("line %d: duplicate %s", lineNumber, bodyLengthCapDirective)
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, bodyLengthCapDirective))
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return true, fmt.Errorf("line %d: %s wants a positive integer, got %q", lineNumber, bodyLengthCapDirective, raw)
		}
		result.BodyLengthCap = value
		return true, nil
	}
	if strings.HasPrefix(line, bodyLengthDirective+"\t") {
		fields := strings.Split(strings.TrimPrefix(line, bodyLengthDirective+"\t"), "\t")
		if len(fields) != 3 {
			return true, fmt.Errorf("line %d: want %s\\t<path>\\t<symbol>\\t<lines>", lineNumber, bodyLengthDirective)
		}
		lines, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil || lines < 1 {
			return true, fmt.Errorf("line %d: body length %q is not a positive integer", lineNumber, fields[2])
		}
		entryKey := key(fields[0], fields[1])
		if _, duplicate := result.BodyLengths[entryKey]; duplicate {
			return true, fmt.Errorf("line %d: duplicate body-length entry %s %s", lineNumber, fields[0], fields[1])
		}
		result.BodyLengths[entryKey] = lines
		return true, nil
	}
	if !strings.HasPrefix(line, bodyLengthJustification+"\t") {
		return false, nil
	}
	fields := strings.SplitN(strings.TrimPrefix(line, bodyLengthJustification+"\t"), "\t", 4)
	if len(fields) != 4 {
		return true, fmt.Errorf("line %d: want %s\\t<path>\\t<symbol>\\t<target>\\t<why>", lineNumber, bodyLengthJustification)
	}
	target, err := strconv.Atoi(strings.TrimSpace(fields[2]))
	if err != nil || target < 1 {
		return true, fmt.Errorf("line %d: body-length justification target %q is not a positive integer", lineNumber, fields[2])
	}
	reason := strings.TrimSpace(fields[3])
	if reason == "" {
		return true, fmt.Errorf("line %d: %s needs a justification", lineNumber, bodyLengthJustification)
	}
	entryKey := key(fields[0], fields[1])
	if _, duplicate := result.BodyJustifications[entryKey]; duplicate {
		return true, fmt.Errorf("line %d: duplicate body-length justification for %s %s", lineNumber, fields[0], fields[1])
	}
	result.BodyJustifications[entryKey] = justification{Target: target, Reason: reason}
	return true, nil
}

func parseJustificationLine(line string, lineNumber int, result *baseline) (bool, error) {
	if strings.HasPrefix(line, entryJustificationDirective+"\t") {
		fields := strings.SplitN(strings.TrimPrefix(line, entryJustificationDirective+"\t"), "\t", 4)
		if len(fields) != 4 {
			return true, fmt.Errorf("line %d: want %s\\t<path>\\t<symbol>\\t<target>\\t<why>", lineNumber, entryJustificationDirective)
		}
		target, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil || target < 1 {
			return true, fmt.Errorf("line %d: justification target %q is not a positive integer", lineNumber, fields[2])
		}
		reason := strings.TrimSpace(fields[3])
		if reason == "" {
			return true, fmt.Errorf("line %d: %s needs a justification", lineNumber, entryJustificationDirective)
		}
		entryKey := key(fields[0], fields[1])
		if _, duplicate := result.EntryJustifications[entryKey]; duplicate {
			return true, fmt.Errorf("line %d: duplicate justification for %s %s", lineNumber, fields[0], fields[1])
		}
		result.EntryJustifications[entryKey] = justification{Target: target, Reason: reason}
		return true, nil
	}
	if !strings.HasPrefix(line, budgetJustificationDirective+"\t") {
		return false, nil
	}
	fields := strings.SplitN(strings.TrimPrefix(line, budgetJustificationDirective+"\t"), "\t", 2)
	if len(fields) != 2 {
		return true, fmt.Errorf("line %d: want %s\\t<target>\\t<why>", lineNumber, budgetJustificationDirective)
	}
	target, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil || target < 0 {
		return true, fmt.Errorf("line %d: budget justification target %q is not a non-negative integer", lineNumber, fields[0])
	}
	reason := strings.TrimSpace(fields[1])
	if reason == "" {
		return true, fmt.Errorf("line %d: %s needs a justification", lineNumber, budgetJustificationDirective)
	}
	if result.RatchetJustification != nil {
		return true, fmt.Errorf("line %d: duplicate %s", lineNumber, budgetJustificationDirective)
	}
	result.RatchetJustification = &justification{Target: target, Reason: reason}
	return true, nil
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
		recorded, baselined := base.Entries[entryKey]
		if current.Allowed && !baselined {
			continue
		}
		seen[entryKey] = true
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
	bodyProblems, bodyNotes := evaluateBodyLengths(functions, base, limits.body)
	problems = append(problems, bodyProblems...)
	notes = append(notes, bodyNotes...)

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

func evaluateBodyLengths(functions []function, base baseline, limit int) (problems, notes []string) {
	if base.BodyLengthCap < 0 {
		return nil, nil
	}
	if base.BodyLengthCap != limit {
		return []string{fmt.Sprintf("complexitygate: body-length cap is %d in the baseline, but the configured fixed cap is %d", base.BodyLengthCap, limit)}, nil
	}
	invalidEntries := make(map[string]bool)
	for entryKey, recorded := range base.BodyLengths {
		ceiling, seeded := bodyLengthSeedCeilings[entryKey]
		if seeded && recorded <= ceiling {
			continue
		}
		invalidEntries[entryKey] = true
		path, symbol, _ := strings.Cut(entryKey, "\t")
		problems = append(problems, fmt.Sprintf(
			"complexitygate: body-length baseline entry %s %s at %d was not admitted by the sealed migration inventory",
			path, symbol, recorded,
		))
	}
	seen := make(map[string]bool)
	for _, current := range functions {
		if current.BodyLines < limit {
			continue
		}
		entryKey := key(current.Path, current.Symbol)
		recorded, baselined := base.BodyLengths[entryKey]
		seen[entryKey] = true
		if invalidEntries[entryKey] {
			continue
		}
		switch {
		case !baselined:
			problems = append(problems, fmt.Sprintf(
				"%s:%d: %s: body length %d is at or above the fixed cap of %d and is not in the baseline",
				current.Path, current.Line, current.Symbol, current.BodyLines, limit,
			))
		case current.BodyLines > recorded:
			problems = append(problems, fmt.Sprintf(
				"%s:%d: %s: body length grew from the baselined %d to %d",
				current.Path, current.Line, current.Symbol, recorded, current.BodyLines,
			))
		}
	}
	for entryKey := range base.BodyLengths {
		if seen[entryKey] {
			continue
		}
		path, symbol, _ := strings.Cut(entryKey, "\t")
		notes = append(notes, fmt.Sprintf(
			"complexitygate: stale body-length baseline entry %s %s is now below the cap; drop it with `make complexity-update`",
			path, symbol,
		))
	}
	return problems, notes
}

func writeBaseline(path string, functions []function, limits thresholds, previous, candidate *baseline) error {
	next := baselineForFunctions(functions, limits, previous, candidate)
	if err := validateBaselineUpdate(previous, next); err != nil {
		return err
	}
	var builder strings.Builder
	builder.WriteString("# Complexity and body-length baseline for test/complexitygate (#4231, #4847).\n")
	builder.WriteString("# Generated by `make complexity-update`; do not hand-edit the scores.\n")
	builder.WriteString("#\n")
	fmt.Fprintf(&builder, "# Entries are every function at or above the hard cap of %d, keyed by\n", limits.hardCap)
	builder.WriteString("# <path>\\t<symbol>\\t<complexity>. The key is path+symbol, so moving a\n")
	builder.WriteString("# function to another file does not create headroom.\n")
	fmt.Fprintf(&builder, "# %s is how many functions may sit at or above %d.\n", budgetDirective, limits.ratchet)
	builder.WriteString("# Score or budget increases require an exact-target justification directive.\n")
	fmt.Fprintf(&builder, "# %s\\t<path>\\t<symbol>\\t<target>\\t<why>\n", entryJustificationDirective)
	fmt.Fprintf(&builder, "# %s\\t<target>\\t<why>\n", budgetJustificationDirective)
	fmt.Fprintf(&builder, "# Functions at or above %d lines are frozen in a separate path+symbol baseline.\n", limits.body)
	builder.WriteString("# Once this tier exists, newly oversized functions cannot be added by the updater.\n")
	fmt.Fprintf(&builder, "# %s <lines>\n", bodyLengthCapDirective)
	fmt.Fprintf(&builder, "# %s\\t<path>\\t<symbol>\\t<lines>\n", bodyLengthDirective)
	fmt.Fprintf(&builder, "# %s\\t<path>\\t<symbol>\\t<target>\\t<why>\n", bodyLengthJustification)
	if next.RatchetJustification != nil {
		fmt.Fprintf(&builder, "%s\t%d\t%s\n", budgetJustificationDirective, next.RatchetJustification.Target, next.RatchetJustification.Reason)
	}
	fmt.Fprintf(&builder, "%s %d\n", budgetDirective, next.RatchetBudget)
	for _, scored := range functions {
		entryKey := key(scored.Path, scored.Symbol)
		score, included := next.Entries[entryKey]
		if !included {
			continue
		}
		if reason, ok := next.EntryJustifications[entryKey]; ok {
			fmt.Fprintf(&builder, "%s\t%s\t%s\t%d\t%s\n", entryJustificationDirective, scored.Path, scored.Symbol, reason.Target, reason.Reason)
		}
		fmt.Fprintf(&builder, "%s\t%s\t%d\n", scored.Path, scored.Symbol, score)
	}
	fmt.Fprintf(&builder, "%s %d\n", bodyLengthCapDirective, next.BodyLengthCap)
	for _, scored := range functions {
		entryKey := key(scored.Path, scored.Symbol)
		lines, included := next.BodyLengths[entryKey]
		if !included {
			continue
		}
		if reason, ok := next.BodyJustifications[entryKey]; ok {
			fmt.Fprintf(&builder, "%s\t%s\t%s\t%d\t%s\n", bodyLengthJustification, scored.Path, scored.Symbol, reason.Target, reason.Reason)
		}
		fmt.Fprintf(&builder, "%s\t%s\t%s\t%d\n", bodyLengthDirective, scored.Path, scored.Symbol, lines)
	}
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}

func baselineForFunctions(functions []function, limits thresholds, previous, candidate *baseline) baseline {
	next := baseline{
		Entries:             make(map[string]int),
		EntryJustifications: make(map[string]justification),
		RatchetBudget:       countAtLeast(functions, limits.ratchet),
		BodyLengths:         make(map[string]int),
		BodyJustifications:  make(map[string]justification),
		BodyLengthCap:       limits.body,
	}
	for _, scored := range functions {
		if scored.Complexity < limits.hardCap {
			continue
		}
		entryKey := key(scored.Path, scored.Symbol)
		_, alreadyBaselined := baselineEntry(previous, entryKey)
		if scored.Allowed && !alreadyBaselined {
			continue
		}
		next.Entries[entryKey] = scored.Complexity
	}
	seedBodyLengths := previous == nil || previous.BodyLengthCap < 0
	for _, measured := range functions {
		if measured.BodyLines < limits.body {
			continue
		}
		entryKey := key(measured.Path, measured.Symbol)
		_, alreadyBaselined := bodyLengthEntry(previous, entryKey)
		_, seeded := bodyLengthSeedCeilings[entryKey]
		if (seedBodyLengths && seeded) || alreadyBaselined {
			next.BodyLengths[entryKey] = measured.BodyLines
		}
	}
	if candidate == nil {
		return next
	}
	if reason := candidate.RatchetJustification; reason != nil && reason.Target == next.RatchetBudget {
		copy := *reason
		next.RatchetJustification = &copy
	}
	for entryKey, reason := range candidate.EntryJustifications {
		if score, ok := next.Entries[entryKey]; ok && reason.Target == score {
			next.EntryJustifications[entryKey] = reason
		}
	}
	for entryKey, reason := range candidate.BodyJustifications {
		if lines, ok := next.BodyLengths[entryKey]; ok && reason.Target == lines {
			next.BodyJustifications[entryKey] = reason
		}
	}
	return next
}

func baselineEntry(current *baseline, entryKey string) (int, bool) {
	if current == nil {
		return 0, false
	}
	score, ok := current.Entries[entryKey]
	return score, ok
}

func bodyLengthEntry(current *baseline, entryKey string) (int, bool) {
	if current == nil {
		return 0, false
	}
	lines, ok := current.BodyLengths[entryKey]
	return lines, ok
}

func validateBaselineUpdate(current *baseline, next baseline) error {
	if current == nil {
		return nil
	}
	var problems []string
	if current.BodyLengthCap >= 0 && next.BodyLengthCap != current.BodyLengthCap {
		problems = append(problems, fmt.Sprintf("body-length cap is fixed at %d and cannot change to %d", current.BodyLengthCap, next.BodyLengthCap))
	}
	if next.RatchetBudget > current.RatchetBudget && !justifies(next.RatchetJustification, next.RatchetBudget) {
		problems = append(problems, fmt.Sprintf(
			"ratchet budget would grow from %d to %d without %s\\t%d\\t<why>",
			current.RatchetBudget, next.RatchetBudget, budgetJustificationDirective, next.RatchetBudget,
		))
	}
	for entryKey, nextScore := range next.Entries {
		previousScore, existed := current.Entries[entryKey]
		if !existed || nextScore <= previousScore {
			continue
		}
		if reason, ok := next.EntryJustifications[entryKey]; ok && reason.Target == nextScore && strings.TrimSpace(reason.Reason) != "" {
			continue
		}
		path, symbol, _ := strings.Cut(entryKey, "\t")
		problems = append(problems, fmt.Sprintf(
			"%s %s would grow from %d to %d without %s\\t%s\\t%s\\t%d\\t<why>",
			path, symbol, previousScore, nextScore, entryJustificationDirective, path, symbol, nextScore,
		))
	}
	for entryKey, nextLines := range next.BodyLengths {
		previousLines, existed := current.BodyLengths[entryKey]
		if !existed || nextLines <= previousLines {
			continue
		}
		if reason, ok := next.BodyJustifications[entryKey]; ok && reason.Target == nextLines && strings.TrimSpace(reason.Reason) != "" {
			continue
		}
		path, symbol, _ := strings.Cut(entryKey, "\t")
		problems = append(problems, fmt.Sprintf(
			"%s %s body length would grow from %d to %d without %s\\t%s\\t%s\\t%d\\t<why>",
			path, symbol, previousLines, nextLines, bodyLengthJustification, path, symbol, nextLines,
		))
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New(strings.Join(problems, "\n"))
}

func justifies(reason *justification, target int) bool {
	return reason != nil && reason.Target == target && strings.TrimSpace(reason.Reason) != ""
}
