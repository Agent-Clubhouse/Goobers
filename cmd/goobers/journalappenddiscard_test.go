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
	// method value inside a function literal, an embedded/promoted method, and
	// the common file whose receiver is an InstanceLog only on Windows.
	// Arbitrary/misleading Append types and all propagated results remain legal.
	if len(findings) != 14 {
		t.Fatalf("findings = %d, want 14:\n%s", len(findings), strings.Join(findings, "\n"))
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
	var findings []string
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		methodValues := instanceAppendMethodValues(function.Body, info)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch statement := node.(type) {
			case *ast.AssignStmt:
				if len(statement.Lhs) == 1 && isBlankIdentifier(statement.Lhs[0]) && len(statement.Rhs) == 1 && isInstanceAppendCall(statement.Rhs[0], info, methodValues) {
					findings = append(findings, physicalPosition(files, statement.Pos()))
				}
			case *ast.DeclStmt:
				if declaration, ok := statement.Decl.(*ast.GenDecl); ok {
					for _, spec := range declaration.Specs {
						values, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, name := range values.Names {
							if name.Name == "_" && i < len(values.Values) && isInstanceAppendCall(values.Values[i], info, methodValues) {
								findings = append(findings, physicalPosition(files, values.Pos()))
							}
						}
					}
				}
			case *ast.ExprStmt:
				if isInstanceAppendCall(statement.X, info, methodValues) {
					findings = append(findings, physicalPosition(files, statement.Pos()))
				}
			case *ast.DeferStmt:
				if isInstanceAppendCall(statement.Call, info, methodValues) {
					findings = append(findings, physicalPosition(files, statement.Pos()))
				}
			case *ast.GoStmt:
				if isInstanceAppendCall(statement.Call, info, methodValues) {
					findings = append(findings, physicalPosition(files, statement.Pos()))
				}
			}
			return true
		})
	}
	return findings
}

type methodValueFlow struct {
	destination types.Object
	source      types.Object
	seed        bool
}

// instanceAppendMethodValues computes a conservative whole-function fixed
// point. A destination is tainted if any assignment can give it an instance
// Append method value; a later assignment cannot erase a path that remains
// possible through a branch or closure.
func instanceAppendMethodValues(body *ast.BlockStmt, info *types.Info) map[types.Object]bool {
	var flows []methodValueFlow
	ast.Inspect(body, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			flows = append(flows, methodValueFlows(statement.Lhs, statement.Rhs, info)...)
		case *ast.DeclStmt:
			declaration, ok := statement.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, spec := range declaration.Specs {
				if values, ok := spec.(*ast.ValueSpec); ok {
					flows = append(flows, methodValueFlows(identsToExpressions(values.Names), values.Values, info)...)
				}
			}
		}
		return true
	})

	tainted := make(map[types.Object]bool)
	changed := true
	for changed {
		changed = false
		for _, flow := range flows {
			if tainted[flow.destination] || (!flow.seed && !tainted[flow.source]) {
				continue
			}
			tainted[flow.destination] = true
			changed = true
		}
	}
	return tainted
}

func methodValueFlows(lhs, rhs []ast.Expr, info *types.Info) []methodValueFlow {
	var flows []methodValueFlow
	for i, left := range lhs {
		identifier, ok := unparen(left).(*ast.Ident)
		if !ok || identifier.Name == "_" || i >= len(rhs) {
			continue
		}
		destination := info.ObjectOf(identifier)
		if destination == nil {
			continue
		}
		right := unparen(rhs[i])
		if isInstanceAppendMethod(right, info) {
			flows = append(flows, methodValueFlow{destination: destination, seed: true})
			continue
		}
		if source, ok := right.(*ast.Ident); ok {
			if object := info.ObjectOf(source); object != nil {
				flows = append(flows, methodValueFlow{destination: destination, source: object})
			}
		}
	}
	return flows
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
