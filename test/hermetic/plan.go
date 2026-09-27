package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// testPlan is one `go test` invocation. An unsharded run is a single plan; a
// shard is one plan for its whole packages plus one per split package it
// holds pieces of, and the plans run concurrently so a serial piece overlaps
// the rest of the shard instead of running after it.
//
// A split piece is resolved lazily (resolve != nil): enumerating the package
// compiles its test binary, so the piece's own goroutine does that while the
// shard's whole packages are already running.
type testPlan struct {
	label        string
	testArgs     []string
	timingOutput string
	resolve      func() (testPlan, error)
	tests        int
	totalTests   int
	filter       string
	testSeconds  float64
	totalSeconds float64
}

// toolEnvironment is the isolated go executable and environment every
// go command the tier runs uses, so `go test -list` compiles the same cached
// binary the real run then reuses.
type toolEnvironment struct {
	goPath string
	root   string
	env    []string
}

// shardPlans computes this shard's plans from `go list ./...`, the checked-in
// package weights, and the split table.
func shardPlans(tools toolEnvironment, testArgs []string, spec shardSpec) ([]testPlan, shardSelection, error) {
	if !containsArg(testArgs, "./...") {
		return nil, shardSelection{}, errors.New("--shard requires a ./... package spec in the go test arguments")
	}
	list := exec.Command(tools.goPath, "list", "./...")
	list.Dir = tools.root
	list.Env = tools.env
	output, err := list.Output()
	if err != nil {
		return nil, shardSelection{}, fmt.Errorf("list packages for sharding: %w", err)
	}
	packages := strings.Fields(string(output))
	if len(packages) == 0 {
		return nil, shardSelection{}, errors.New("go list ./... returned no packages to shard")
	}
	weights, err := loadShardWeights(tools.root)
	if err != nil {
		return nil, shardSelection{}, err
	}
	splits, err := loadShardSplits(tools.root)
	if err != nil {
		return nil, shardSelection{}, err
	}
	selection := assignShards(shardItems(packages, weights, splits), spec.total)[spec.index-1]
	if len(selection.packages) == 0 && len(selection.pieces) == 0 {
		return nil, selection, fmt.Errorf("shard %d/%d selected nothing from %d packages", spec.index, spec.total, len(packages))
	}
	return selectionPlans(tools, testArgs, selection, splits), selection, nil
}

func selectionPlans(tools toolEnvironment, testArgs []string, selection shardSelection, splits shardSplits) []testPlan {
	var plans []testPlan
	if len(selection.packages) > 0 {
		sorted := append([]string(nil), selection.packages...)
		sort.Strings(sorted)
		plans = append(plans, testPlan{
			label:    fmt.Sprintf("%d whole packages", len(sorted)),
			testArgs: replacePackageSpec(testArgs, sorted),
		})
	}
	for _, pkg := range piecePackages(selection.pieces) {
		split, pieces := splits.Packages[pkg], piecesOf(selection.pieces, pkg)
		plans = append(plans, testPlan{
			label: pieceLabel(pkg, split, pieces),
			resolve: func() (testPlan, error) {
				names, err := listPackageTests(tools.goPath, tools.root, tools.env, testArgs, pkg)
				if err != nil {
					return testPlan{}, err
				}
				return piecePlan(testArgs, pkg, names, split, pieces)
			},
		})
	}
	return plans
}

func pieceLabel(pkg string, split splitPackage, pieces []int) string {
	labels := make([]string, 0, len(pieces))
	for _, piece := range pieces {
		labels = append(labels, fmt.Sprintf("%d/%d", piece, split.Pieces))
	}
	return fmt.Sprintf("%s piece %s", pkg, strings.Join(labels, "+"))
}

// resolvePlan enumerates a lazy piece; a ready plan is returned unchanged.
func resolvePlan(plan testPlan) (testPlan, error) {
	if plan.resolve == nil {
		return plan, nil
	}
	resolved, err := plan.resolve()
	if err != nil {
		return testPlan{}, fmt.Errorf("%s: %w", plan.label, err)
	}
	resolved.timingOutput = plan.timingOutput
	return resolved, nil
}

// piecePlan merges every piece of pkg this shard holds into one invocation.
func piecePlan(testArgs []string, pkg string, names []string, split splitPackage, pieces []int) (testPlan, error) {
	groups := partitionTests(names, split)
	var selected []string
	for _, piece := range pieces {
		selected = append(selected, groups[piece-1]...)
	}
	sort.Strings(selected)
	all := groupsUnion(groups)
	args, err := pieceArgs(testArgs, pkg, selected, all)
	if err != nil {
		return testPlan{}, err
	}
	return testPlan{
		label:        pieceLabel(pkg, split, pieces),
		testArgs:     args,
		tests:        len(selected),
		totalTests:   len(all),
		filter:       filterSummary(args),
		testSeconds:  split.sumSeconds(selected),
		totalSeconds: split.sumSeconds(all),
	}, nil
}

// filterSummary names the test filter a piece uses and its size.
func filterSummary(args []string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "-run" || args[index] == "-skip" {
			return fmt.Sprintf("%s pattern of %d bytes", args[index], len(args[index+1]))
		}
	}
	return "unfiltered"
}

func groupsUnion(groups [][]string) []string {
	var all []string
	for _, group := range groups {
		all = append(all, group...)
	}
	return all
}

func piecePackages(pieces []shardItem) []string {
	seen := make(map[string]struct{})
	var packages []string
	for _, item := range pieces {
		if _, ok := seen[item.pkg]; !ok {
			seen[item.pkg] = struct{}{}
			packages = append(packages, item.pkg)
		}
	}
	sort.Strings(packages)
	return packages
}

func piecesOf(pieces []shardItem, pkg string) []int {
	var result []int
	for _, item := range pieces {
		if item.pkg == pkg {
			result = append(result, item.piece)
		}
	}
	sort.Ints(result)
	return result
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// assignTimingOutputs gives each plan of a sharded run its own timing file
// (<stem>.part<N><ext>), since each plan is a separate go test process.
func assignTimingOutputs(plans []testPlan, output string) {
	if output == "" {
		return
	}
	extension := filepath.Ext(output)
	stem := strings.TrimSuffix(output, extension)
	for index := range plans {
		plans[index].timingOutput = fmt.Sprintf("%s.part%d%s", stem, index+1, extension)
	}
}

func describeShard(destination io.Writer, spec shardSpec, selection shardSelection, plans []testPlan) {
	_, _ = fmt.Fprintf(destination, "hermetic tier: shard %d/%d runs %d whole packages and %d split pieces (predicted %.0fs of package weight)\n",
		spec.index, spec.total, len(selection.packages), len(selection.pieces), selection.seconds)
	for _, plan := range plans {
		if plan.resolve == nil {
			describePlan(destination, plan)
		}
	}
}

func describePlan(destination io.Writer, plan testPlan) {
	if plan.totalTests == 0 {
		_, _ = fmt.Fprintf(destination, "hermetic tier:   %s\n", plan.label)
		return
	}
	_, _ = fmt.Fprintf(destination, "hermetic tier:   %s: %d of %d top-level tests (%.0fs of %.0fs measured; %s)\n",
		plan.label, plan.tests, plan.totalTests, plan.testSeconds, plan.totalSeconds, plan.filter)
}

// describeDryRun resolves every piece (compiling its test binary) and prints
// the complete plan without running any test.
func describeDryRun(destination io.Writer, plans []testPlan) error {
	for _, plan := range plans {
		if plan.resolve == nil {
			continue
		}
		resolved, err := resolvePlan(plan)
		if err != nil {
			return err
		}
		describePlan(destination, resolved)
	}
	return nil
}

// executePlans runs every plan concurrently and reports whether all passed.
// Output is forwarded a whole line at a time so concurrent processes never
// interleave inside a line.
func executePlans(tools toolEnvironment, base invocation, plans []testPlan, stdout, stderr io.Writer, collector *diagnosticCollector) bool {
	sharedOut := &lockedWriter{destination: stdout}
	sharedErr := &lockedWriter{destination: stderr}
	results := make([]error, len(plans))
	var group sync.WaitGroup
	for index, plan := range plans {
		group.Add(1)
		go func() {
			defer group.Done()
			results[index] = executePlan(tools, base, plan, sharedOut, sharedErr, collector)
		}()
	}
	group.Wait()
	passed := true
	for index, err := range results {
		if err != nil {
			passed = false
			if len(plans) > 1 {
				_, _ = fmt.Fprintf(stderr, "hermetic tier: %s failed: %v\n", plans[index].label, err)
			}
		}
	}
	return passed
}

func executePlan(tools toolEnvironment, base invocation, plan testPlan, stdout, stderr *lockedWriter, collector *diagnosticCollector) error {
	if plan.resolve != nil {
		resolved, err := resolvePlan(plan)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "hermetic tier: %v\n", err)
			return err
		}
		describePlan(stdout, resolved)
		plan = resolved
	}
	current := base
	current.testArgs = plan.testArgs
	current.timingOutput = plan.timingOutput
	command := exec.Command(tools.goPath, goCommandArgs(current)...)
	command.Dir = tools.root
	command.Env = tools.env
	outLines := &lineWriter{destination: stdout}
	errLines := &lineWriter{destination: stderr}
	stdoutWriter := &diagnosticWriter{destination: outLines, collector: collector}
	stderrWriter := &diagnosticWriter{destination: errLines, collector: collector}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	err := command.Run()
	stdoutWriter.flush()
	stderrWriter.flush()
	outLines.flush()
	errLines.flush()
	return err
}

type lockedWriter struct {
	mu          sync.Mutex
	destination io.Writer
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.destination.Write(data)
}

// lineWriter holds a partial trailing line until it is completed, so each
// write to the shared destination is a run of whole lines.
type lineWriter struct {
	destination io.Writer
	pending     []byte
}

func (w *lineWriter) Write(data []byte) (int, error) {
	w.pending = append(w.pending, data...)
	end := bytes.LastIndexByte(w.pending, '\n')
	if end < 0 {
		return len(data), nil
	}
	complete := w.pending[:end+1]
	if _, err := w.destination.Write(complete); err != nil {
		return 0, err
	}
	w.pending = append([]byte(nil), w.pending[end+1:]...)
	return len(data), nil
}

func (w *lineWriter) flush() {
	if len(w.pending) > 0 {
		_, _ = w.destination.Write(w.pending)
		w.pending = nil
	}
}
