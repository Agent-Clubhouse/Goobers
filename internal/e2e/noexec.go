package e2e

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
)

// Violation names one place the forbidden pod-exec pattern was found.
type Violation struct {
	Path string
	Line int
	Text string
}

func (v Violation) String() string { return fmt.Sprintf("%s:%d: %s", v.Path, v.Line, v.Text) }

// kubectlExecPattern matches "kubectl" followed, anywhere later on the same
// line (allowing flags/subcommands in between — "kubectl -n gaggle exec
// pod -- sh" is exactly as forbidden as the bare form), by "exec" as its own
// word. It is built with \b word boundaries so "kubectl execute-something"
// (not a real subcommand) does not false-positive, matching goobernetes-
// smoke.md §5 rule 1's target: the `kubectl exec` SUBCOMMAND, "or equivalent
// pod-exec" per the #3517 task — `kubectl debug` (also a pod-shell escape
// hatch) is caught by the second pattern below.
var (
	kubectlExecPattern  = regexp.MustCompile(`\bkubectl\b(\s+\S+)*\s+\bexec\b`)
	kubectlDebugPattern = regexp.MustCompile(`\bkubectl\b(\s+\S+)*\s+\bdebug\b`)
)

// matchLine reports whether line contains either forbidden pattern.
// Deliberately returns a bool rather than a descriptive string: a string
// constant naming the pattern would itself have to spell out the two-word
// phrase this whole guard exists to forbid, inside a STRING LITERAL —
// exactly what ScanGoStringLiteralsForKubectlExec below would then (function
// correctly, but uselessly) flag in its own package.
func matchLine(line string) bool {
	return kubectlExecPattern.MatchString(line) || kubectlDebugPattern.MatchString(line)
}

// ScanTextForKubectlExec scans free text (an operator procedure transcript,
// S7's observer) line by line for the forbidden pattern. This is a plain
// text scan — a transcript is a literal command-history record, not Go
// source, so there is no "comment vs. executed code" distinction to make
// here the way there is for ScanGoStringLiteralsForKubectlExec.
func ScanTextForKubectlExec(text string) []Violation {
	if text == "" {
		return nil
	}
	var violations []Violation
	scanner := bufio.NewScanner(strings.NewReader(text))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if matchLine(line) {
			violations = append(violations, Violation{Path: "<transcript>", Line: lineNo, Text: strings.TrimSpace(line)})
		}
	}
	return violations
}

// ScanGoStringLiteralsForKubectlExec is the §5 rule 1 / D3 guard over the
// harness's OWN procedure/commands: "kubectl exec appearing anywhere in the
// procedure is a defect... the smoke is re-run after the fix" — applied
// here to this package's own source, since goobernetes-smoke.md §5 rule 1
// makes this a property the harness must hold about itself, not only about
// a future live transcript.
//
// It walks every .go file under root in fsys and inspects STRING LITERALS
// only (via go/parser's AST, the same convention test/nophonehome already
// establishes for this repo: scan constructs that could actually be
// EXECUTED, not doc comments that merely discuss the rule in prose — a
// comment explaining "never run kubectl exec" is not itself a procedure
// step, and treating it as one would make this guard untestable against its
// own documentation). Deliberately excludes _test.go files: this guard's own
// tests intentionally construct a violation as a string to prove detection
// works (see noexec_test.go), and a test fixture proving the guard catches
// the pattern is not the harness's procedure either.
func ScanGoStringLiteralsForKubectlExec(fsys fs.FS, root string) ([]Violation, error) {
	var violations []Violation
	err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				value = lit.Value
			}
			if matchLine(value) {
				pos := fset.Position(lit.Pos())
				violations = append(violations, Violation{Path: path, Line: pos.Line, Text: strings.TrimSpace(value)})
			}
			return true
		})
		return nil
	})
	if err != nil {
		return violations, err
	}
	return violations, nil
}
