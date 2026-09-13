package main

import (
	"bytes"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/goobers/goobers/internal/testgit"
)

// TestIntentionalJournalAppendDiscardsUseObservableHelper keeps the explicit
// best-effort boundary complete. Its source universe comes from Git's index,
// not the working tree: nested or untracked .clubhouse worktrees must not make
// this repository invariant depend on a developer's local filesystem.
func TestIntentionalJournalAppendDiscardsUseObservableHelper(t *testing.T) {
	repo := repositoryRoot(t)
	for _, finding := range discardedInstanceAppends(t, repo, trackedProductionGoFiles(t, repo)) {
		t.Errorf("%s: discarded instance-log Append must use AppendBestEffort", finding)
	}
}

func TestTrackedProductionGoFilesUsesCanonicalGitInventory(t *testing.T) {
	repo := t.TempDir()
	runTestGit(t, repo, "init", "--quiet")
	writeFixtureFile(t, repo, "tracked.go", "package fixture\n")
	writeFixtureFile(t, repo, "sub/also.go", "package sub\n")
	writeFixtureFile(t, repo, "ignored_test.go", "package fixture\n")
	writeFixtureFile(t, repo, "vendor/example/vendor.go", "package example\n")
	runTestGit(t, repo, "add", "tracked.go", "sub/also.go", "ignored_test.go", "vendor/example/vendor.go")
	writeFixtureFile(t, repo, ".clubhouse/agents/untracked.go", "package agents\n")
	writeFixtureFile(t, repo, "untracked.go", "package fixture\n")

	got := trackedProductionGoFiles(t, repo)
	want := []string{"sub/also.go", "tracked.go"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tracked production files = %q, want %q", got, want)
	}
}

func TestDiscardedInstanceAppendGuardIsTypeAwareAndCoversHostileShapes(t *testing.T) {
	repo := t.TempDir()
	runTestGit(t, repo, "init", "--quiet")
	writeFixtureFile(t, repo, "go.mod", "module github.com/goobers/goobers\n\ngo 1.25\n")
	writeFixtureFile(t, repo, "internal/journal/journal.go", `package journal
type Event struct{}
type InstanceLog struct{}
func (*InstanceLog) Append(Event) error { return nil }
func (*InstanceLog) AppendBestEffort(Event) {}
`)
	writeFixtureFile(t, repo, "internal/livejournal/livejournal.go", `package livejournal
import "github.com/goobers/goobers/internal/journal"
type InstanceAppender interface { Append(journal.Event) error }
`)
	writeFixtureFile(t, repo, "fixture.go", `package fixture
import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)
type arbitrary struct{}
func (arbitrary) Append(journal.Event) error { return nil }
type InstanceAppender interface { Append(journal.Event) error }
var global *journal.InstanceLog
var packageClosure = func() { global.Append(journal.Event{}) }
var unrelatedClosure = func() {
	var other arbitrary
	other.Append(journal.Event{})
}
func hostile(parameter *journal.InstanceLog, intended livejournal.InstanceAppender, misleading InstanceAppender, other arbitrary) error {
	parameter.Append(journal.Event{})
	_ = (parameter.Append)(journal.Event{})
	var _ = parameter.Append(journal.Event{})
	defer ((parameter)).Append(journal.Event{})
	go parameter.Append(journal.Event{})
	var declared *journal.InstanceLog = parameter
	_ = declared.Append(journal.Event{})
	method := (parameter.Append)
	_ = method(journal.Event{})
	var declaredMethod = parameter.Append
	declaredMethod(journal.Event{})
	intendedMethod := intended.Append
	_ = intendedMethod(journal.Event{})
	direct := parameter.Append
	copied := direct
	_ = copied(journal.Event{})
	var branched func(journal.Event) error
	if parameter != nil { branched = parameter.Append } else { branched = other.Append }
	_ = branched(journal.Event{})
	func() {
		inside := parameter.Append
		insideCopy := inside
		insideCopy(journal.Event{})
	}()
	type promoted struct { *journal.InstanceLog }
	value := promoted{parameter}
	value.Append(journal.Event{})
	misleading.Append(journal.Event{})
	other.Append(journal.Event{})
	_ = other.Append(journal.Event{})
	otherDirect := other.Append
	otherCopied := otherDirect
	otherCopied(journal.Event{})
	if err := parameter.Append(journal.Event{}); err != nil { return err }
	err := parameter.Append(journal.Event{})
	if err != nil { return err }
	parameter.AppendBestEffort(journal.Event{})
	return parameter.Append(journal.Event{})
}
func orderedReassignment(parameter *journal.InstanceLog, other arbitrary) error {
	method := other.Append
	method(journal.Event{})
	method = parameter.Append
	return method(journal.Event{})
}
`)
	writeFixtureFile(t, repo, "platform.go", `package fixture
import "github.com/goobers/goobers/internal/journal"
func platform(value platformAppender) { value.Append(journal.Event{}) }
`)
	writeFixtureFile(t, repo, "platform_darwin.go", `//go:build darwin
package fixture
type platformAppender = arbitrary
`)
	writeFixtureFile(t, repo, "platform_windows.go", `//go:build windows
package fixture
import "github.com/goobers/goobers/internal/journal"
type platformAppender = *journal.InstanceLog
`)
	writeFixtureFile(t, repo, "platform_other.go", `//go:build !darwin && !windows
package fixture
type platformAppender = arbitrary
`)
	runTestGit(t, repo, "add", "go.mod", "internal/journal/journal.go", "internal/livejournal/livejournal.go", "fixture.go",
		"platform.go", "platform_darwin.go", "platform_windows.go", "platform_other.go")

	findings := discardedInstanceAppends(t, repo, trackedProductionGoFiles(t, repo))
	// The original nine shapes plus copied and branch-merged method values, a
	// nested method value, an embedded/promoted method, the common file whose
	// receiver is an InstanceLog only on Windows, and a package-scope closure.
	// Arbitrary/misleading Append types and all propagated results remain legal.
	if len(findings) != 15 {
		t.Fatalf("findings = %d, want 15:\n%s", len(findings), strings.Join(findings, "\n"))
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate structural test")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(current)))
}

func trackedProductionGoFiles(t *testing.T, repo string) []string {
	t.Helper()
	cmd := testgit.Command("-C", repo, "ls-files", "--cached", "-z", "--", "*.go")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list tracked Go sources: %v", err)
	}
	var files []string
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		path := filepath.ToSlash(filepath.Clean(string(raw)))
		if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
			t.Fatalf("git returned non-repository path %q", path)
		}
		if strings.HasSuffix(path, "_test.go") || path == "vendor" || strings.HasPrefix(path, "vendor/") || strings.Contains(path, "/vendor/") {
			continue
		}
		files = append(files, path)
	}
	sort.Strings(files)
	return files
}

func discardedInstanceAppends(t *testing.T, repo string, tracked []string) []string {
	t.Helper()
	trackedSet := make(map[string]struct{}, len(tracked))
	candidates := make(map[string]struct{})
	for _, relative := range tracked {
		trackedSet[relative] = struct{}{}
		source, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		if bytes.Contains(source, []byte("Append")) {
			candidates[relative] = struct{}{}
		}
	}

	compiled := make(map[string]bool, len(candidates))
	findingSet := make(map[string]struct{})
	for _, goos := range uniqueStrings(runtime.GOOS, "linux", "darwin", "windows") {
		cfg := &packages.Config{
			Dir: repo,
			Env: append(os.Environ(), "GOOS="+goos, "CGO_ENABLED=0"),
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
				packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		}
		loaded, err := packages.Load(cfg, "./...")
		if err != nil {
			t.Fatalf("load production packages for %s: %v", goos, err)
		}
		for _, pkg := range loaded {
			if len(pkg.Errors) != 0 && packageContainsCandidate(repo, pkg.CompiledGoFiles, candidates) {
				t.Fatalf("type-check candidate package %s for %s: %s", pkg.PkgPath, goos, pkg.Errors[0])
			}
			for i, syntax := range pkg.Syntax {
				absolute := pkg.CompiledGoFiles[i]
				relative, err := filepath.Rel(repo, absolute)
				if err != nil {
					continue
				}
				relative = filepath.ToSlash(filepath.Clean(relative))
				if _, tracked := trackedSet[relative]; !tracked {
					continue
				}
				if _, candidate := candidates[relative]; !candidate {
					continue
				}
				compiled[relative] = true
				for _, finding := range discardedAppendsInFile(pkg.Fset, syntax, pkg.TypesInfo) {
					findingSet[finding] = struct{}{}
				}
			}
		}
	}
	var missing []string
	for path := range candidates {
		if !compiled[path] {
			missing = append(missing, path)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		t.Fatalf("type-check tracked Append sources on supported platforms: %s", strings.Join(missing, ", "))
	}
	findings := make([]string, 0, len(findingSet))
	for finding := range findingSet {
		findings = append(findings, finding)
	}
	sort.Strings(findings)
	return findings
}

func packageContainsCandidate(repo string, absoluteFiles []string, candidates map[string]struct{}) bool {
	for _, absolute := range absoluteFiles {
		relative, err := filepath.Rel(repo, absolute)
		if err != nil {
			continue
		}
		if _, ok := candidates[filepath.ToSlash(filepath.Clean(relative))]; ok {
			return true
		}
	}
	return false
}

func discardedAppendsInFile(files *token.FileSet, parsed *ast.File, info *types.Info) []string {
	findings := make(map[string]struct{})
	for _, body := range journalFunctionBodies(parsed) {
		analyzeAliasBlock(body.List, make(aliasState), files, info, findings)
	}
	result := make([]string, 0, len(findings))
	for finding := range findings {
		result = append(result, finding)
	}
	return result
}

func journalFunctionBodies(parsed *ast.File) []*ast.BlockStmt {
	var bodies []*ast.BlockStmt
	for _, declaration := range parsed.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if typed.Body != nil {
				bodies = append(bodies, typed.Body)
			}
		case *ast.GenDecl:
			ast.Inspect(typed, func(node ast.Node) bool {
				function, ok := node.(*ast.FuncLit)
				if !ok {
					return true
				}
				bodies = append(bodies, function.Body)
				return false
			})
		}
	}
	return bodies
}

type aliasState map[types.Object]bool

func analyzeAliasBlock(statements []ast.Stmt, state aliasState, files *token.FileSet, info *types.Info, findings map[string]struct{}) aliasState {
	for _, statement := range statements {
		state = analyzeAliasStatement(statement, state, files, info, findings)
	}
	return state
}

func analyzeAliasStatement(statement ast.Stmt, state aliasState, files *token.FileSet, info *types.Info, findings map[string]struct{}) aliasState {
	switch typed := statement.(type) {
	case *ast.BlockStmt:
		return analyzeAliasBlock(typed.List, state, files, info, findings)
	case *ast.IfStmt:
		if typed.Init != nil {
			state = analyzeAliasStatement(typed.Init, state, files, info, findings)
		}
		thenState := analyzeAliasBlock(typed.Body.List, cloneAliasState(state), files, info, findings)
		elseState := cloneAliasState(state)
		if typed.Else != nil {
			elseState = analyzeAliasStatement(typed.Else, elseState, files, info, findings)
		}
		return joinAliasStates(thenState, elseState)
	case *ast.ForStmt:
		if typed.Init != nil {
			state = analyzeAliasStatement(typed.Init, state, files, info, findings)
		}
		return analyzeAliasLoop(typed.Body.List, typed.Post, state, files, info, findings)
	case *ast.RangeStmt:
		loopState := cloneAliasState(state)
		clearAliasTarget(loopState, typed.Key, info)
		clearAliasTarget(loopState, typed.Value, info)
		return analyzeAliasLoop(typed.Body.List, nil, loopState, files, info, findings)
	case *ast.SwitchStmt:
		if typed.Init != nil {
			state = analyzeAliasStatement(typed.Init, state, files, info, findings)
		}
		return analyzeAliasClauses(typed.Body.List, state, files, info, findings)
	case *ast.TypeSwitchStmt:
		if typed.Init != nil {
			state = analyzeAliasStatement(typed.Init, state, files, info, findings)
		}
		return analyzeAliasClauses(typed.Body.List, state, files, info, findings)
	case *ast.SelectStmt:
		return analyzeAliasClauses(typed.Body.List, state, files, info, findings)
	case *ast.LabeledStmt:
		return analyzeAliasStatement(typed.Stmt, state, files, info, findings)
	}

	recordDiscardedAliasCall(statement, state, files, info, findings)
	analyzeNestedClosures(statement, state, files, info, findings)
	switch typed := statement.(type) {
	case *ast.AssignStmt:
		assignAliasValues(state, typed.Lhs, typed.Rhs, info)
	case *ast.DeclStmt:
		if declaration, ok := typed.Decl.(*ast.GenDecl); ok {
			for _, spec := range declaration.Specs {
				if values, ok := spec.(*ast.ValueSpec); ok {
					assignAliasValues(state, identsToExpressions(values.Names), values.Values, info)
				}
			}
		}
	}
	return state
}

func analyzeAliasLoop(body []ast.Stmt, post ast.Stmt, entry aliasState, files *token.FileSet, info *types.Info, findings map[string]struct{}) aliasState {
	current := cloneAliasState(entry)
	for {
		exit := analyzeAliasBlock(body, cloneAliasState(current), files, info, findings)
		if post != nil {
			exit = analyzeAliasStatement(post, exit, files, info, findings)
		}
		next := joinAliasStates(entry, exit)
		if equalAliasStates(current, next) {
			return next
		}
		current = next
	}
}

func analyzeAliasClauses(clauses []ast.Stmt, entry aliasState, files *token.FileSet, info *types.Info, findings map[string]struct{}) aliasState {
	result := cloneAliasState(entry)
	for _, statement := range clauses {
		clause, ok := statement.(*ast.CaseClause)
		if !ok {
			if communication, ok := statement.(*ast.CommClause); ok {
				result = joinAliasStates(result, analyzeAliasBlock(communication.Body, cloneAliasState(entry), files, info, findings))
			}
			continue
		}
		result = joinAliasStates(result, analyzeAliasBlock(clause.Body, cloneAliasState(entry), files, info, findings))
	}
	return result
}

func recordDiscardedAliasCall(statement ast.Stmt, state aliasState, files *token.FileSet, info *types.Info, findings map[string]struct{}) {
	var expression ast.Expr
	switch typed := statement.(type) {
	case *ast.AssignStmt:
		if len(typed.Lhs) == 1 && isBlankIdentifier(typed.Lhs[0]) && len(typed.Rhs) == 1 {
			expression = typed.Rhs[0]
		}
	case *ast.DeclStmt:
		if declaration, ok := typed.Decl.(*ast.GenDecl); ok {
			for _, spec := range declaration.Specs {
				if values, ok := spec.(*ast.ValueSpec); ok {
					for i, name := range values.Names {
						if name.Name == "_" && i < len(values.Values) && isInstanceAppendCall(values.Values[i], info, state) {
							findings[physicalPosition(files, values.Pos())] = struct{}{}
						}
					}
				}
			}
		}
	case *ast.ExprStmt:
		expression = typed.X
	case *ast.DeferStmt:
		expression = typed.Call
	case *ast.GoStmt:
		expression = typed.Call
	}
	if expression != nil && isInstanceAppendCall(expression, info, state) {
		findings[physicalPosition(files, statement.Pos())] = struct{}{}
	}
}

func analyzeNestedClosures(node ast.Node, state aliasState, files *token.FileSet, info *types.Info, findings map[string]struct{}) {
	ast.Inspect(node, func(node ast.Node) bool {
		function, ok := node.(*ast.FuncLit)
		if !ok {
			return true
		}
		analyzeAliasBlock(function.Body.List, cloneAliasState(state), files, info, findings)
		return false
	})
}

func assignAliasValues(state aliasState, lhs, rhs []ast.Expr, info *types.Info) {
	values := make([]bool, len(lhs))
	for i := range lhs {
		if i < len(rhs) {
			values[i] = aliasExpressionTainted(rhs[i], state, info)
		}
	}
	for i, left := range lhs {
		identifier, ok := unparen(left).(*ast.Ident)
		if !ok || identifier.Name == "_" {
			continue
		}
		if object := info.ObjectOf(identifier); object != nil {
			state[object] = values[i]
		}
	}
}

func aliasExpressionTainted(expression ast.Expr, state aliasState, info *types.Info) bool {
	expression = unparen(expression)
	if isInstanceAppendMethod(expression, info) {
		return true
	}
	identifier, ok := expression.(*ast.Ident)
	return ok && state[info.ObjectOf(identifier)]
}

func clearAliasTarget(state aliasState, expression ast.Expr, info *types.Info) {
	if identifier, ok := unparen(expression).(*ast.Ident); ok {
		state[info.ObjectOf(identifier)] = false
	}
}

func cloneAliasState(state aliasState) aliasState {
	result := make(aliasState, len(state))
	for object, tainted := range state {
		result[object] = tainted
	}
	return result
}

func joinAliasStates(states ...aliasState) aliasState {
	result := make(aliasState)
	for _, state := range states {
		for object, tainted := range state {
			result[object] = result[object] || tainted
		}
	}
	return result
}

func equalAliasStates(left, right aliasState) bool {
	for object, tainted := range left {
		if right[object] != tainted {
			return false
		}
	}
	for object, tainted := range right {
		if left[object] != tainted {
			return false
		}
	}
	return true
}

func isInstanceAppendCall(expression ast.Expr, info *types.Info, aliases map[types.Object]bool) bool {
	call, ok := unparen(expression).(*ast.CallExpr)
	if !ok {
		return false
	}
	switch function := unparen(call.Fun).(type) {
	case *ast.SelectorExpr:
		return isInstanceAppendSelection(function, info)
	case *ast.Ident:
		return aliases[info.ObjectOf(function)]
	default:
		return false
	}
}

func isInstanceAppendMethod(expression ast.Expr, info *types.Info) bool {
	selector, ok := unparen(expression).(*ast.SelectorExpr)
	return ok && isInstanceAppendSelection(selector, info)
}

func isInstanceAppendSelection(selector *ast.SelectorExpr, info *types.Info) bool {
	selection := info.Selections[selector]
	if selector.Sel.Name != "Append" || selection == nil {
		return false
	}
	if isInstanceAppenderType(selection.Recv()) {
		return true
	}
	// For an embedded *InstanceLog, Recv is the promoting outer struct. The
	// selected method object retains Append's declaring receiver.
	function, ok := selection.Obj().(*types.Func)
	if !ok {
		return false
	}
	signature, ok := function.Type().(*types.Signature)
	return ok && signature.Recv() != nil && isInstanceAppenderType(signature.Recv().Type())
}

func physicalPosition(files *token.FileSet, position token.Pos) string {
	return files.PositionFor(position, false).String()
}

func isInstanceAppenderType(value types.Type) bool {
	for {
		switch typed := value.(type) {
		case *types.Pointer:
			value = typed.Elem()
		case *types.Named:
			object := typed.Obj()
			if object.Pkg() == nil {
				return false
			}
			packagePath := object.Pkg().Path()
			if object.Name() == "InstanceLog" && strings.HasSuffix(packagePath, "/internal/journal") {
				return true
			}
			// This is an audited registry of interfaces whose production contract
			// is specifically the instance journal. Matching exact package/type
			// identity avoids treating arbitrary or run-journal Append methods as
			// instance-log losses.
			return (packagePath == "github.com/goobers/goobers/internal/livejournal" && object.Name() == "InstanceAppender") ||
				(packagePath == "github.com/goobers/goobers/internal/webhook" && object.Name() == "InstanceJournal") ||
				(packagePath == "github.com/goobers/goobers/cmd/goobers" && object.Name() == "workerDivergenceAppender")
		default:
			return false
		}
	}
}

func unparen(expression ast.Expr) ast.Expr {
	for {
		paren, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = paren.X
	}
}

func isBlankIdentifier(expression ast.Expr) bool {
	identifier, ok := unparen(expression).(*ast.Ident)
	return ok && identifier.Name == "_"
}

func identsToExpressions(identifiers []*ast.Ident) []ast.Expr {
	expressions := make([]ast.Expr, len(identifiers))
	for i := range identifiers {
		expressions[i] = identifiers[i]
	}
	return expressions
}

func uniqueStrings(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	var result []string
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func runTestGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := testgit.Command(append([]string{"-C", repo}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func writeFixtureFile(t *testing.T, repo, relative, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
