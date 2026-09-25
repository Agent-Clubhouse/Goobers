package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// shardSplitsPath holds the packages the Linux race shards run in pieces, and
// the per-test measurements used to balance those pieces.
//
// Package-level LPT cannot place a package on more than one runner, so one
// package that outweighs a fair share of the suite (cmd/goobers: ~3,500
// serial tests, ~20 min under -race) fixes the critical path no matter how
// many shards there are. A split package is instead scheduled as `pieces`
// equal-weight items; the shard that receives piece p enumerates the
// package's top-level tests with `go test -list`, partitions them by LPT over
// the per-test weights below, and runs its piece with an anchored `-run`.
const shardSplitsPath = ".github/unit-shard-splits.json"

// maxRunPatternBytes keeps one piece's -run expression well under Linux's
// 128 KiB single-argument limit (MAX_ARG_STRLEN). A piece that outgrows it
// fails loudly: raise the package's piece count.
const maxRunPatternBytes = 96 * 1024

type shardSplits struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Source        shardSplitsSource       `json:"source"`
	Packages      map[string]splitPackage `json:"packages"`
}

type shardSplitsSource struct {
	Run         int64    `json:"run"`
	Commit      string   `json:"commit"`
	GeneratedAt string   `json:"generatedAt"`
	TimingJobs  []string `json:"timingJobs"`
	Platform    string   `json:"platform"`
}

// splitPackage is one split package. Tests holds relative per-test seconds:
// only proportions inside the package matter, because each piece is scheduled
// at packageSeconds/pieces and the per-test weights only balance the pieces
// against one another.
type splitPackage struct {
	Pieces int                `json:"pieces"`
	Tests  map[string]float64 `json:"tests"`
}

// shardItem is one schedulable unit: a whole package (piece == 0) or piece
// 1..pieces of a split package.
type shardItem struct {
	pkg     string
	piece   int
	seconds float64
}

// shardSelection is what one shard runs.
type shardSelection struct {
	packages []string
	pieces   []shardItem
	seconds  float64
}

// testMillis converts per-test seconds to integer milliseconds. Every runner
// holding a piece of a package must derive the identical partition, so the
// partition arithmetic is done in integers: a float sum over Go's randomized
// map iteration order can differ in its last bit between runners and flip an
// LPT tie, which would drop one test and double-run another.
func testMillis(seconds float64) int64 {
	return int64(math.Round(seconds * 1000))
}

// defaultTestMillis is the weight an unmeasured (new or renamed) test gets:
// the package's mean measured test, so new tests spread across pieces instead
// of piling into one.
func (p splitPackage) defaultTestMillis() int64 {
	var total int64
	for _, seconds := range p.Tests {
		total += testMillis(seconds)
	}
	if len(p.Tests) == 0 || total <= 0 {
		return 1000
	}
	if mean := total / int64(len(p.Tests)); mean > 0 {
		return mean
	}
	return 1
}

// weigher returns the millisecond weight of any test name in the package.
func (p splitPackage) weigher() func(string) int64 {
	fallback := p.defaultTestMillis()
	return func(name string) int64 {
		if seconds, ok := p.Tests[name]; ok {
			return testMillis(seconds)
		}
		return fallback
	}
}

func (p splitPackage) sumSeconds(names []string) float64 {
	weigh := p.weigher()
	var total int64
	for _, name := range names {
		total += weigh(name)
	}
	return float64(total) / 1000
}

func loadShardSplits(root string) (shardSplits, error) {
	path := filepath.Join(root, shardSplitsPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return shardSplits{}, fmt.Errorf("read shard splits %s: %w", path, err)
	}
	var splits shardSplits
	if err := json.Unmarshal(data, &splits); err != nil {
		return shardSplits{}, fmt.Errorf("parse shard splits %s: %w", path, err)
	}
	if err := splits.validate(); err != nil {
		return shardSplits{}, fmt.Errorf("shard splits %s: %w", path, err)
	}
	return splits, nil
}

func (s shardSplits) validate() error {
	if s.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schemaVersion %d", s.SchemaVersion)
	}
	if s.Source.Run <= 0 || !validLowerHexSHA(s.Source.Commit) || len(s.Source.TimingJobs) == 0 {
		return errors.New("source must identify a positive run, a full commit SHA, and the timing jobs measured")
	}
	for pkg, split := range s.Packages {
		if strings.TrimSpace(pkg) == "" || split.Pieces < 2 {
			return fmt.Errorf("package %q must be split into at least 2 pieces", pkg)
		}
		for name, seconds := range split.Tests {
			if !validTestName(name) || seconds < 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) {
				return fmt.Errorf("package %q test %q must be a top-level test with a finite non-negative duration", pkg, name)
			}
		}
	}
	return nil
}

func validLowerHexSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// validTestName accepts the top-level names `go test -list` prints for tests,
// examples, and fuzz targets. Subtests follow their parent, so a name with a
// slash never appears here.
func validTestName(name string) bool {
	if name == "" || strings.ContainsAny(name, "/ \t") {
		return false
	}
	for _, prefix := range []string{"Test", "Example", "Fuzz"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// shardItems expands the package list into schedulable items. A split package
// becomes `pieces` items of equal weight; everything else stays whole.
func shardItems(pkgs []string, weights shardWeights, splits shardSplits) []shardItem {
	items := make([]shardItem, 0, len(pkgs))
	seen := make(map[string]struct{}, len(pkgs))
	for _, pkg := range pkgs {
		if _, duplicate := seen[pkg]; duplicate {
			continue
		}
		seen[pkg] = struct{}{}
		split, isSplit := splits.Packages[pkg]
		if !isSplit {
			items = append(items, shardItem{pkg: pkg, seconds: weights.packageSeconds(pkg)})
			continue
		}
		share := weights.packageSeconds(pkg) / float64(split.Pieces)
		for piece := 1; piece <= split.Pieces; piece++ {
			items = append(items, shardItem{pkg: pkg, piece: piece, seconds: share})
		}
	}
	return items
}

// assignShards distributes items longest-processing-time-first: measured slow
// items are placed before small ones fill the gaps. Ties break by package and
// piece so every runner computes the same plan.
func assignShards(items []shardItem, total int) []shardSelection {
	ordered := append([]shardItem(nil), items...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.seconds != right.seconds {
			return left.seconds > right.seconds
		}
		if left.pkg != right.pkg {
			return left.pkg < right.pkg
		}
		return left.piece < right.piece
	})
	shards := make([]shardSelection, total)
	for _, item := range ordered {
		target := 0
		for index := 1; index < total; index++ {
			if shards[index].seconds < shards[target].seconds {
				target = index
			}
		}
		if item.piece == 0 {
			shards[target].packages = append(shards[target].packages, item.pkg)
		} else {
			shards[target].pieces = append(shards[target].pieces, item)
		}
		shards[target].seconds += item.seconds
	}
	return shards
}

// partitionTests splits a package's enumerated top-level tests into
// split.Pieces disjoint, exhaustive groups by LPT over the per-test weights.
// Every enumerated test lands in exactly one group, measured or not; the
// result depends only on the set of names, never on their order.
func partitionTests(names []string, split splitPackage) [][]string {
	unique := make(map[string]struct{}, len(names))
	for _, name := range names {
		unique[name] = struct{}{}
	}
	ordered := make([]string, 0, len(unique))
	for name := range unique {
		ordered = append(ordered, name)
	}
	weigh := split.weigher()
	sort.Slice(ordered, func(i, j int) bool {
		left, right := weigh(ordered[i]), weigh(ordered[j])
		if left != right {
			return left > right
		}
		return ordered[i] < ordered[j]
	})
	groups := make([][]string, split.Pieces)
	totals := make([]int64, split.Pieces)
	for _, name := range ordered {
		target := 0
		for index := 1; index < len(groups); index++ {
			if totals[index] < totals[target] {
				target = index
			}
		}
		groups[target] = append(groups[target], name)
		totals[target] += weigh(name)
	}
	for _, group := range groups {
		sort.Strings(group)
	}
	return groups
}

// runPattern anchors an exact alternation of top-level names. go test splits
// -run on '/' into per-level patterns; with a single level every subtest of a
// selected test still runs, and no other top-level test matches.
func runPattern(names []string) string {
	quoted := make([]string, len(names))
	for index, name := range names {
		quoted[index] = regexp.QuoteMeta(name)
	}
	return "^(?:" + strings.Join(quoted, "|") + ")$"
}

// parseTestList reads `go test -list` output: one top-level name per line,
// followed by the package summary line. Benchmarks are dropped because the
// unit suite never runs them.
func parseTestList(output string) []string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		name := strings.TrimSpace(line)
		if validTestName(name) {
			names = append(names, name)
		}
	}
	return names
}

// listPackageTests enumerates the top-level tests the unit suite would run in
// pkg, compiled with the same go test flags (so the build tags, -race, and
// the resulting cached binary are those of the real run).
func listPackageTests(goPath, root string, env, testArgs []string, pkg string) ([]string, error) {
	args := append([]string{"test", "-list", "."}, replacePackageSpec(testArgs, []string{pkg})...)
	list := exec.Command(goPath, args...)
	list.Dir = root
	list.Env = env
	output, err := list.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list tests in %s: %w\n%s", pkg, err, output)
	}
	names := parseTestList(string(output))
	if len(names) == 0 {
		return nil, fmt.Errorf("list tests in %s: no top-level tests found:\n%s", pkg, output)
	}
	return names, nil
}

// replacePackageSpec swaps the `./...` package spec for packages.
func replacePackageSpec(testArgs, packages []string) []string {
	result := make([]string, 0, len(testArgs)+len(packages))
	for _, arg := range testArgs {
		if arg == "./..." {
			result = append(result, packages...)
			continue
		}
		result = append(result, arg)
	}
	return result
}

// pieceArgs builds the go test arguments that run exactly the selected
// tests of pkg, given every test the package enumerated.
//
// The filter is whichever of two equivalent forms is shorter: `-run` over the
// selection, or `-skip` over its complement (the tests other shards run). LPT
// packs the many tiny tests into whichever piece still has room, so one piece
// can hold most of the names; its complement is then far shorter than the
// selection and keeps the argument inside the kernel's single-argument limit.
// Both forms are exact over the enumerated names; every runner enumerates the
// same commit with the same flags, so the pieces agree on the partition.
func pieceArgs(testArgs []string, pkg string, selected, all []string) ([]string, error) {
	for _, arg := range testArgs {
		if arg == "-run" || strings.HasPrefix(arg, "-run=") || arg == "-skip" || strings.HasPrefix(arg, "-skip=") {
			return nil, fmt.Errorf("cannot split %s: the go test arguments already carry %s", pkg, arg)
		}
	}
	complement := subtractNames(all, selected)
	if len(complement) == 0 {
		return replacePackageSpec(testArgs, []string{pkg}), nil
	}
	flag, pattern := "-run", runPattern(selected)
	if skip := runPattern(complement); len(skip) < len(pattern) {
		flag, pattern = "-skip", skip
	}
	if len(pattern) > maxRunPatternBytes {
		return nil, fmt.Errorf("%s piece selects %d of %d tests in a %d-byte %s pattern (limit %d): raise its piece count in %s",
			pkg, len(selected), len(all), len(pattern), flag, maxRunPatternBytes, shardSplitsPath)
	}
	return replacePackageSpec(testArgs, []string{flag, pattern, pkg}), nil
}

func subtractNames(all, remove []string) []string {
	removed := make(map[string]struct{}, len(remove))
	for _, name := range remove {
		removed[name] = struct{}{}
	}
	var result []string
	for _, name := range all {
		if _, skip := removed[name]; !skip {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}
