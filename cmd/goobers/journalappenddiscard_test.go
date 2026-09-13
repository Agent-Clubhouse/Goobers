package main

import (
	"bytes"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

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
func loopBreak(parameter *journal.InstanceLog, other arbitrary) {
	method := other.Append
	for {
		method = parameter.Append
		break
		method = other.Append
	}
	method(journal.Event{})
}
func switchFallthrough(parameter *journal.InstanceLog, other arbitrary, choice int) {
	method := other.Append
	switch choice {
	case 0:
		method = parameter.Append
		fallthrough
	case 1:
		method(journal.Event{})
	}
}
func closureCapture(parameter *journal.InstanceLog, other arbitrary) {
	method := other.Append
	closure := func() { method(journal.Event{}) }
	method = parameter.Append
	closure()
}
func closureBeforeReassignment(parameter *journal.InstanceLog, other arbitrary) {
	method := other.Append
	closure := func() { method(journal.Event{}) }
	closure()
	method = parameter.Append
	_ = method
}
func terminatingBranch(parameter *journal.InstanceLog, other arbitrary, stop bool) {
	method := other.Append
	if stop {
		method = parameter.Append
		return
	}
	method(journal.Event{})
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
	if len(findings) != 18 {
		t.Fatalf("findings = %d, want 18:\n%s", len(findings), strings.Join(findings, "\n"))
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
				packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
				packages.NeedImports | packages.NeedDeps,
		}
		loaded, err := packages.Load(cfg, "./...")
		if err != nil {
			t.Fatalf("load production packages for %s: %v", goos, err)
		}
		for _, pkg := range loaded {
			if len(pkg.Errors) != 0 && packageContainsCandidate(repo, pkg.CompiledGoFiles, candidates) {
				t.Fatalf("type-check candidate package %s for %s: %s", pkg.PkgPath, goos, pkg.Errors[0])
			}
			for i := range pkg.Syntax {
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
			}
		}
		program, _ := ssautil.Packages(loaded, ssa.InstantiateGenerics)
		program.Build()
		for _, finding := range discardedInstanceAppendsSSA(program, repo, candidates) {
			findingSet[finding] = struct{}{}
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

type ssaAppendAnalysis struct {
	stores       map[ssa.Value][]ssa.Value
	bindings     map[*ssa.FreeVar][]ssa.Value
	closureCalls map[*ssa.Function][]ssa.CallInstruction
}

func discardedInstanceAppendsSSA(program *ssa.Program, repo string, candidates map[string]struct{}) []string {
	functions := ssautil.AllFunctions(program)
	analysis := &ssaAppendAnalysis{
		stores:       make(map[ssa.Value][]ssa.Value),
		bindings:     make(map[*ssa.FreeVar][]ssa.Value),
		closureCalls: make(map[*ssa.Function][]ssa.CallInstruction),
	}
	for function := range functions {
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				switch typed := instruction.(type) {
				case *ssa.Store:
					analysis.stores[typed.Addr] = append(analysis.stores[typed.Addr], typed.Val)
				case *ssa.MakeClosure:
					callee, ok := typed.Fn.(*ssa.Function)
					if !ok {
						continue
					}
					for i, free := range callee.FreeVars {
						if i < len(typed.Bindings) {
							analysis.bindings[free] = append(analysis.bindings[free], typed.Bindings[i])
						}
					}
				}
			}
		}
	}
	for function := range functions {
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				call, ok := instruction.(ssa.CallInstruction)
				if !ok || call.Common().IsInvoke() {
					continue
				}
				for closure := range analysis.closureFunctions(call.Common().Value, make(map[ssa.Value]bool)) {
					analysis.closureCalls[closure] = append(analysis.closureCalls[closure], call)
				}
			}
		}
	}

	findings := make(map[string]struct{})
	for function := range functions {
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				call, ok := instruction.(ssa.CallInstruction)
				if !ok || !analysis.callMayAppendInstance(call.Common(), make(map[ssa.Value]bool)) || !discardedCallResult(call) {
					continue
				}
				position := program.Fset.PositionFor(call.Common().Pos(), false)
				relative, err := filepath.Rel(repo, position.Filename)
				if err != nil {
					continue
				}
				relative = filepath.ToSlash(filepath.Clean(relative))
				if _, candidate := candidates[relative]; candidate {
					findings[position.String()] = struct{}{}
				}
			}
		}
	}
	result := make([]string, 0, len(findings))
	for finding := range findings {
		result = append(result, finding)
	}
	return result
}

func discardedCallResult(call ssa.CallInstruction) bool {
	value := call.Value()
	if value == nil {
		return true
	}
	referrers := value.Referrers()
	if referrers == nil {
		return true
	}
	for _, instruction := range *referrers {
		if _, debugOnly := instruction.(*ssa.DebugRef); !debugOnly {
			return false
		}
	}
	return true
}

func (a *ssaAppendAnalysis) callMayAppendInstance(call *ssa.CallCommon, seen map[ssa.Value]bool) bool {
	if call.IsInvoke() {
		return call.Method.Name() == "Append" && (isInstanceAppenderType(call.Value.Type()) || isInstanceAppendFunction(call.Method))
	}
	return a.valueMayBeInstanceAppend(call.Value, seen)
}

func (a *ssaAppendAnalysis) valueMayBeInstanceAppend(value ssa.Value, seen map[ssa.Value]bool) bool {
	if value == nil || seen[value] {
		return false
	}
	seen[value] = true
	switch typed := value.(type) {
	case *ssa.Function:
		if object, ok := typed.Object().(*types.Func); ok && isInstanceAppendFunction(object) {
			return true
		}
		if typed.Synthetic == "" {
			return false
		}
		for _, block := range typed.Blocks {
			for _, instruction := range block.Instrs {
				if call, ok := instruction.(ssa.CallInstruction); ok && a.callMayAppendInstance(call.Common(), seen) {
					return true
				}
			}
		}
	case *ssa.MakeClosure:
		return a.valueMayBeInstanceAppend(typed.Fn, seen)
	case *ssa.Phi:
		for _, edge := range typed.Edges {
			if a.valueMayBeInstanceAppend(edge, seen) {
				return true
			}
		}
	case *ssa.UnOp:
		if typed.Op == token.MUL {
			for _, stored := range a.storedValues(typed.X, make(map[ssa.Value]bool)) {
				if a.valueMayBeInstanceAppend(stored, seen) {
					return true
				}
			}
		}
	case *ssa.ChangeType:
		return a.valueMayBeInstanceAppend(typed.X, seen)
	case *ssa.Convert:
		return a.valueMayBeInstanceAppend(typed.X, seen)
	case *ssa.ChangeInterface:
		return a.valueMayBeInstanceAppend(typed.X, seen)
	case *ssa.MakeInterface:
		return a.valueMayBeInstanceAppend(typed.X, seen)
	case *ssa.Extract:
		return a.valueMayBeInstanceAppend(typed.Tuple, seen)
	}
	return false
}

func (a *ssaAppendAnalysis) storedValues(pointer ssa.Value, seen map[ssa.Value]bool) []ssa.Value {
	if pointer == nil || seen[pointer] {
		return nil
	}
	seen[pointer] = true
	values := append([]ssa.Value(nil), a.stores[pointer]...)
	switch typed := pointer.(type) {
	case *ssa.FreeVar:
		for _, binding := range a.bindings[typed] {
			calls := a.closureCalls[typed.Parent()]
			if len(calls) == 0 {
				values = append(values, a.storedValues(binding, seen)...)
				continue
			}
			for _, call := range calls {
				values = append(values, reachingStoredValues(binding, call)...)
			}
		}
	case *ssa.Phi:
		for _, edge := range typed.Edges {
			values = append(values, a.storedValues(edge, seen)...)
		}
	case *ssa.ChangeType:
		values = append(values, a.storedValues(typed.X, seen)...)
	case *ssa.Convert:
		values = append(values, a.storedValues(typed.X, seen)...)
	}
	return values
}

func (a *ssaAppendAnalysis) closureFunctions(value ssa.Value, seen map[ssa.Value]bool) map[*ssa.Function]struct{} {
	result := make(map[*ssa.Function]struct{})
	if value == nil || seen[value] {
		return result
	}
	seen[value] = true
	switch typed := value.(type) {
	case *ssa.MakeClosure:
		if function, ok := typed.Fn.(*ssa.Function); ok {
			result[function] = struct{}{}
		}
	case *ssa.Function:
		if typed.Parent() != nil {
			result[typed] = struct{}{}
		}
	case *ssa.Phi:
		for _, edge := range typed.Edges {
			mergeFunctions(result, a.closureFunctions(edge, seen))
		}
	case *ssa.UnOp:
		if typed.Op == token.MUL {
			for _, stored := range a.storedValues(typed.X, make(map[ssa.Value]bool)) {
				mergeFunctions(result, a.closureFunctions(stored, seen))
			}
		}
	case *ssa.ChangeType:
		mergeFunctions(result, a.closureFunctions(typed.X, seen))
	case *ssa.Convert:
		mergeFunctions(result, a.closureFunctions(typed.X, seen))
	}
	return result
}

func mergeFunctions(destination, source map[*ssa.Function]struct{}) {
	for function := range source {
		destination[function] = struct{}{}
	}
}

func reachingStoredValues(pointer ssa.Value, call ssa.CallInstruction) []ssa.Value {
	target := call.Block()
	if target == nil || target.Parent() == nil {
		return nil
	}
	function := target.Parent()
	in := make(map[*ssa.BasicBlock]map[ssa.Value]struct{}, len(function.Blocks))
	out := make(map[*ssa.BasicBlock]map[ssa.Value]struct{}, len(function.Blocks))
	changed := true
	for changed {
		changed = false
		for i, block := range function.Blocks {
			if i != 0 && len(block.Preds) == 0 {
				continue
			}
			entry := make(map[ssa.Value]struct{})
			for _, predecessor := range block.Preds {
				mergeValues(entry, out[predecessor])
			}
			if !equalValueSets(in[block], entry) {
				in[block] = entry
				changed = true
			}
			exit := transferStoredValues(block.Instrs, pointer, entry, nil)
			if !equalValueSets(out[block], exit) {
				out[block] = exit
				changed = true
			}
		}
	}
	values := transferStoredValues(target.Instrs, pointer, in[target], call)
	result := make([]ssa.Value, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}

func transferStoredValues(instructions []ssa.Instruction, pointer ssa.Value, entry map[ssa.Value]struct{}, stop ssa.Instruction) map[ssa.Value]struct{} {
	state := make(map[ssa.Value]struct{}, len(entry))
	mergeValues(state, entry)
	for _, instruction := range instructions {
		if instruction == stop {
			break
		}
		if store, ok := instruction.(*ssa.Store); ok && store.Addr == pointer {
			state = map[ssa.Value]struct{}{store.Val: {}}
		}
	}
	return state
}

func mergeValues(destination, source map[ssa.Value]struct{}) {
	for value := range source {
		destination[value] = struct{}{}
	}
}

func equalValueSets(left, right map[ssa.Value]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if _, ok := right[value]; !ok {
			return false
		}
	}
	return true
}

func isInstanceAppendFunction(function *types.Func) bool {
	if function == nil || function.Name() != "Append" {
		return false
	}
	signature, ok := function.Type().(*types.Signature)
	return ok && signature.Recv() != nil && isInstanceAppenderType(signature.Recv().Type())
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
