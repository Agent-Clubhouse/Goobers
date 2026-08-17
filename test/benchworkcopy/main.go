// Command benchworkcopy measures working-copy provisioning cost against a
// deterministic synthetic repo fixture (#641 — B0 of docs/design/v2-cloud-scale.md
// §3). It drives the real internal/worktree Manager API — mirror clone, refresh
// fetch, worktree add, teardown — so every B1–B5 provisioning change is measured
// through the exact code path it modifies, and emits machine-readable JSON so
// runs can be diffed and trended.
//
// Usage:
//
//	go run ./test/benchworkcopy [-preset small|medium|large|large-repo] [flags]
//	make bench-workcopy                       # medium preset (~1 min end to end)
//	make bench-large-repo                    # scheduled >=10 GiB pinned gate
//
// The generated fixture is a bare repo parameterized by file count, history
// depth, branch/tag count, and blob-size distribution (compressible text plus
// incompressible assets/ binaries), deterministic for a given seed. The
// benchmark clones it over its file:// URL so both modes measure real packfile
// transport (a plain-path clone would hardlink objects and ignore partial-clone
// filters). The "large" preset is deliberately a sparse rendition for routine
// worktree measurements. "large-repo" is the acceptance corpus: 220k deeply
// nested C#/C++ source files and a >=10 GiB working tree, provisioned in pinned
// mode by the scheduled lane:
//
//	go run ./test/benchworkcopy -preset large-repo -mode pinned
//
// JSON output schema ("goobers.bench-workcopy/v2", one object per run):
//
//	schema         string  schema identifier, bumped on incompatible change
//	elapsedMs      int     total harness wall time
//	goos, goarch   string  host platform
//	gitVersion     string  `git version` output
//	partialClone   bool    mirrors provisioned with blobless partial clone (#646)
//	sparse         array   worktree-mode cones (project.checkout.sparse, #649);
//	                       omitted for a full checkout
//	repoURL        string  benchmarked repo (fixture file:// URL or -repo)
//	fixture        object  generation parameters + generateMs + repoBytes
//	                       (omitted when -repo names an existing repo)
//	coldCloneMs    int     first Manager.WorkingCopy call (mirror clone)
//	mirrorBytes    int     mirror disk bytes after the cold clone
//	warmFetchMs    int     second WorkingCopy call (refresh fetch, no changes)
//	cycles         array   per-cycle {worktreeAddMs, worktreeBytes, teardownMs}
//	worktreeAddMsMedian, teardownMsMedian
//	                int     medians across cycles
//	initToFirstRunMs, secondRunMs, secondRunDeltaMs
//	                int     pinned cold/warm wall measurements (reported, never
//	                        asserted in CI)
//	steadyStateBytes
//	                int     mirror + one pinned workspace + warm build state
//	deepestRelativePathChars, pathBudgetAvailableChars
//	                int     generated depth measured against LR-5 preflight
//	firstRunWorkspaceCreates, secondRunWorkspaceCreates,
//	secondRunCreateDelta, buildStatePreserved
//	                measured work counters pinned by the scheduled gate
//
// The small correctness corpus runs in normal tests. The >=10 GiB corpus runs
// through make bench-large-repo in the scheduled large-repository lane.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/worktree"
)

type cycleResult struct {
	WorktreeAddMs int64 `json:"worktreeAddMs"`
	WorktreeBytes int64 `json:"worktreeBytes"`
	TeardownMs    int64 `json:"teardownMs"`
}

type fixtureReport struct {
	Preset          string `json:"preset,omitempty"`
	Seed            int64  `json:"seed"`
	Files           int    `json:"files"`
	HistoryDepth    int    `json:"historyDepth"`
	Branches        int    `json:"branches"`
	Tags            int    `json:"tags"`
	LargeBlobs      int    `json:"largeBlobs"`
	LargeBlobBytes  int64  `json:"largeBlobBytes"`
	PathDepth       int    `json:"pathDepth"`
	SharedBlobs     int    `json:"sharedBlobs"`
	SharedBlobBytes int64  `json:"sharedBlobBytes"`
	GenerateMs      int64  `json:"generateMs"`
	RepoBytes       int64  `json:"repoBytes"`
}

type report struct {
	Schema                    string         `json:"schema"`
	ElapsedMs                 int64          `json:"elapsedMs"`
	GOOS                      string         `json:"goos"`
	GOARCH                    string         `json:"goarch"`
	GitVersion                string         `json:"gitVersion"`
	PartialClone              bool           `json:"partialClone"`
	Sparse                    []string       `json:"sparse,omitempty"`
	Mode                      string         `json:"mode"`
	RepoURL                   string         `json:"repoURL"`
	Fixture                   *fixtureReport `json:"fixture,omitempty"`
	ColdCloneMs               int64          `json:"coldCloneMs"`
	MirrorBytes               int64          `json:"mirrorBytes"`
	WarmFetchMs               int64          `json:"warmFetchMs"`
	Cycles                    []cycleResult  `json:"cycles"`
	WorktreeAddMsMedian       int64          `json:"worktreeAddMsMedian"`
	TeardownMsMedian          int64          `json:"teardownMsMedian"`
	InitToFirstRunMs          int64          `json:"initToFirstRunMs,omitempty"`
	SecondRunMs               int64          `json:"secondRunMs,omitempty"`
	SecondRunDeltaMs          int64          `json:"secondRunDeltaMs,omitempty"`
	FirstRunWorkspaceBytes    int64          `json:"firstRunWorkspaceBytes,omitempty"`
	SecondRunWorkspaceBytes   int64          `json:"secondRunWorkspaceBytes,omitempty"`
	SteadyStateBytes          int64          `json:"steadyStateBytes,omitempty"`
	DeepestRelativePathChars  int            `json:"deepestRelativePathChars,omitempty"`
	PathBudgetAvailableChars  int            `json:"pathBudgetAvailableChars,omitempty"`
	FirstRunWorkspaceCreates  int            `json:"firstRunWorkspaceCreates"`
	SecondRunWorkspaceCreates int            `json:"secondRunWorkspaceCreates"`
	SecondRunCreateDelta      int            `json:"secondRunCreateDelta"`
	BuildStatePreserved       bool           `json:"buildStatePreserved,omitempty"`
}

const schemaID = "goobers.bench-workcopy/v2"

const (
	largeRepoWorkingTreeFloor = int64(10 << 30)
	largeRepoPathDepthFloor   = 12
	largeRepoDiskCeiling      = int64(12 << 30)
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("benchworkcopy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	preset := fs.String("preset", "small", "fixture preset: small, medium, large, or large-repo")
	mode := fs.String("mode", "worktree", "provisioning mode: worktree or pinned")
	seed := fs.Int64("seed", 1, "fixture PRNG seed (identical seed+parameters => identical repo)")
	files := fs.Int("files", 0, "override fixture file count")
	depth := fs.Int("depth", 0, "override fixture history depth (commit count)")
	branches := fs.Int("branches", -1, "override fixture branch count")
	tags := fs.Int("tags", -1, "override fixture tag count")
	largeBlobs := fs.Int("large-blobs", -1, "override count of incompressible binaries in the fixture")
	largeBlobBytes := fs.Int64("large-blob-bytes", 0, "override the size of each incompressible binary")
	touch := fs.Int("touch-per-commit", 0, "override files rewritten per history commit")
	pathDepth := fs.Int("path-depth", -1, "override source directory nesting depth")
	sharedBlobs := fs.Int("shared-blobs", -1, "override reusable source blob count")
	sharedBlobBytes := fs.Int64("shared-blob-bytes", 0, "override reusable source blob size")
	cycles := fs.Int("cycles", 3, "worktree add/teardown cycles to measure")
	partialClone := fs.Bool("partial-clone", false, "provision mirrors as blobless partial clones (#646) for before/after comparison")
	sparse := fs.String("sparse", "", "comma-separated repo-relative cones (project.checkout.sparse, #649) for before/after comparison; empty is a full checkout")
	fixtureDir := fs.String("fixture", "", "generate (or reuse, if it already exists) the fixture at this path instead of a temp dir")
	keepFixture := fs.Bool("keep-fixture", false, "keep the generated fixture instead of deleting it")
	repo := fs.String("repo", "", "benchmark this existing repo URL instead of generating a fixture")
	baseRef := fs.String("base-ref", "main", "base ref worktrees are provisioned from")
	out := fs.String("out", "", "write the JSON report to this file instead of stdout")
	maxPath := fs.Int("max-path", 320, "LR-5 maximum checkout path length for pinned mode")
	buildAllowance := fs.Int("build-output-allowance", 32, "LR-5 characters reserved for build output")
	buildStateBytes := fs.Int64("build-state-bytes", 64<<20, "ignored warm build state created after the first pinned run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "benchworkcopy: unexpected positional arguments")
		fs.Usage()
		return 2
	}

	spec, ok := presets[*preset]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "benchworkcopy: unknown preset %q (small, medium, large, large-repo)\n", *preset)
		return 2
	}
	if *mode != "worktree" && *mode != "pinned" {
		_, _ = fmt.Fprintln(stderr, "benchworkcopy: -mode must be worktree or pinned")
		return 2
	}
	spec.Seed = *seed
	if *files > 0 {
		spec.Files = *files
	}
	if *depth > 0 {
		spec.HistoryDepth = *depth
	}
	if *branches >= 0 {
		spec.Branches = *branches
	}
	if *tags >= 0 {
		spec.Tags = *tags
	}
	if *largeBlobs >= 0 {
		spec.LargeBlobs = *largeBlobs
	}
	if *largeBlobBytes > 0 {
		spec.LargeBlobBytes = *largeBlobBytes
	}
	if *touch > 0 {
		spec.TouchPerCommit = *touch
	}
	if *pathDepth >= 0 {
		spec.PathDepth = *pathDepth
	}
	if *sharedBlobs >= 0 {
		spec.SharedBlobs = *sharedBlobs
	}
	if *sharedBlobBytes > 0 {
		spec.SharedBlobBytes = *sharedBlobBytes
	}
	if *cycles < 1 {
		_, _ = fmt.Fprintln(stderr, "benchworkcopy: -cycles must be at least 1")
		return 2
	}
	if *maxPath < 1 || *buildAllowance < 0 || *buildStateBytes < 0 {
		_, _ = fmt.Fprintln(stderr, "benchworkcopy: -max-path must be positive; build allowances and state bytes must be non-negative")
		return 2
	}

	var sparseCones []string
	if *sparse != "" {
		sparseCones = strings.Split(*sparse, ",")
	}
	started := time.Now()
	rep, err := benchmark(context.Background(), benchOptions{
		spec:            spec,
		preset:          *preset,
		repo:            *repo,
		fixtureDir:      *fixtureDir,
		keepFixture:     *keepFixture,
		partialClone:    *partialClone,
		sparse:          sparseCones,
		mode:            *mode,
		baseRef:         *baseRef,
		cycles:          *cycles,
		maxPath:         *maxPath,
		buildAllowance:  *buildAllowance,
		buildStateBytes: *buildStateBytes,
	}, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "benchworkcopy: %v\n", err)
		return 1
	}
	rep.ElapsedMs = time.Since(started).Milliseconds()

	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "benchworkcopy: encode report: %v\n", err)
		return 1
	}
	encoded = append(encoded, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, encoded, 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "benchworkcopy: %v\n", err)
			return 1
		}
	} else if _, err := stdout.Write(encoded); err != nil {
		_, _ = fmt.Fprintf(stderr, "benchworkcopy: %v\n", err)
		return 1
	}
	return 0
}

type benchOptions struct {
	spec            fixtureSpec
	preset          string
	repo            string
	fixtureDir      string
	keepFixture     bool
	partialClone    bool
	sparse          []string
	baseRef         string
	cycles          int
	mode            string
	maxPath         int
	buildAllowance  int
	buildStateBytes int64
}

func benchmark(ctx context.Context, opts benchOptions, progress io.Writer) (*report, error) {
	rep := &report{
		Schema:       schemaID,
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		GitVersion:   gitVersion(ctx),
		PartialClone: opts.partialClone,
		Sparse:       opts.sparse,
		Mode:         opts.mode,
	}

	repoURL := opts.repo
	if repoURL == "" {
		dir := opts.fixtureDir
		if dir == "" {
			tmp, err := os.MkdirTemp("", "goobers-bench-fixture-*")
			if err != nil {
				return nil, err
			}
			dir = filepath.Join(tmp, "fixture.git")
			if !opts.keepFixture {
				defer func() { _ = os.RemoveAll(tmp) }()
			}
		}
		fx := &fixtureReport{
			Preset: opts.preset, Seed: opts.spec.Seed,
			Files: opts.spec.Files, HistoryDepth: opts.spec.HistoryDepth,
			Branches: opts.spec.Branches, Tags: opts.spec.Tags,
			LargeBlobs: opts.spec.LargeBlobs, LargeBlobBytes: opts.spec.LargeBlobBytes,
			PathDepth: opts.spec.PathDepth, SharedBlobs: opts.spec.SharedBlobs,
			SharedBlobBytes: opts.spec.SharedBlobBytes,
		}
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			start := time.Now()
			if err := generateFixture(ctx, opts.spec, dir); err != nil {
				return nil, err
			}
			fx.GenerateMs = time.Since(start).Milliseconds()
		} else if err != nil {
			return nil, err
		}
		bytes, err := diskBytes(dir)
		if err != nil {
			return nil, err
		}
		fx.RepoBytes = bytes
		rep.Fixture = fx
		repoURL, err = fileURL(dir)
		if err != nil {
			return nil, err
		}
		_, _ = fmt.Fprintf(progress, "benchworkcopy: fixture %s (%d files, depth %d) ready in %dms (%s)\n",
			opts.preset, opts.spec.Files, opts.spec.HistoryDepth, fx.GenerateMs, humanBytes(fx.RepoBytes))
		if opts.keepFixture {
			_, _ = fmt.Fprintf(progress, "benchworkcopy: fixture kept at %s\n", dir)
		}
	}
	rep.RepoURL = repoURL

	workRoot, err := os.MkdirTemp("", "goobers-bench-workcopies-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(workRoot) }()

	var managerOpts []worktree.ManagerOption
	if opts.partialClone {
		managerOpts = append(managerOpts, worktree.WithPartialClone())
	}
	manager, err := worktree.NewManager(workRoot, managerOpts...)
	if err != nil {
		return nil, err
	}
	if opts.mode == "pinned" {
		manager.SetPathLengthLimits(map[string]worktree.PathLengthLimit{
			repoURL: {
				MaxPathLength:        opts.maxPath,
				BuildOutputAllowance: opts.buildAllowance,
			},
		})
		if err := benchmarkPinned(ctx, manager, repoURL, opts, rep, progress); err != nil {
			return nil, err
		}
		if opts.preset == "large-repo" {
			if err := enforceLargeRepoGates(rep); err != nil {
				return nil, err
			}
		}
		return rep, nil
	}

	start := time.Now()
	mirror, err := manager.WorkingCopy(ctx, repoURL)
	if err != nil {
		return nil, fmt.Errorf("cold clone: %w", err)
	}
	rep.ColdCloneMs = time.Since(start).Milliseconds()
	if rep.MirrorBytes, err = diskBytes(mirror); err != nil {
		return nil, err
	}

	start = time.Now()
	if _, err := manager.WorkingCopy(ctx, repoURL); err != nil {
		return nil, fmt.Errorf("warm fetch: %w", err)
	}
	rep.WarmFetchMs = time.Since(start).Milliseconds()
	_, _ = fmt.Fprintf(progress, "benchworkcopy: cold clone %dms (mirror %s); warm fetch %dms\n",
		rep.ColdCloneMs, humanBytes(rep.MirrorBytes), rep.WarmFetchMs)

	for i := 0; i < opts.cycles; i++ {
		var cycle cycleResult
		start = time.Now()
		wt, err := manager.Create(ctx, worktree.CreateOptions{
			RepoURL: repoURL,
			RunID:   fmt.Sprintf("bench-%d", i),
			BaseRef: opts.baseRef,
			Branch:  fmt.Sprintf("goobers/bench/run%d", i),
			Sparse:  opts.sparse,
		})
		if err != nil {
			return nil, fmt.Errorf("worktree add (cycle %d): %w", i, err)
		}
		cycle.WorktreeAddMs = time.Since(start).Milliseconds()
		if cycle.WorktreeBytes, err = diskBytes(wt.Path); err != nil {
			return nil, err
		}
		start = time.Now()
		if err := wt.Remove(ctx, worktree.RemoveOptions{}); err != nil {
			return nil, fmt.Errorf("teardown (cycle %d): %w", i, err)
		}
		cycle.TeardownMs = time.Since(start).Milliseconds()
		rep.Cycles = append(rep.Cycles, cycle)
	}
	rep.WorktreeAddMsMedian = median(rep.Cycles, func(c cycleResult) int64 { return c.WorktreeAddMs })
	rep.TeardownMsMedian = median(rep.Cycles, func(c cycleResult) int64 { return c.TeardownMs })
	_, _ = fmt.Fprintf(progress, "benchworkcopy: worktree add median %dms; teardown median %dms (%d cycles)\n",
		rep.WorktreeAddMsMedian, rep.TeardownMsMedian, opts.cycles)
	return rep, nil
}

func benchmarkPinned(ctx context.Context, manager *worktree.Manager, repoURL string, opts benchOptions, rep *report, progress io.Writer) error {
	start := time.Now()
	firstLease, err := manager.AcquirePinned(ctx, worktree.PinnedOptions{
		RepoURL: repoURL, RunID: "bench-first", BaseRef: opts.baseRef, Branch: "goobers/bench/pinned",
	})
	if err != nil {
		return fmt.Errorf("acquire first pinned lease: %w", err)
	}
	first := firstLease.Worktree
	rep.InitToFirstRunMs = time.Since(start).Milliseconds()
	rep.FirstRunWorkspaceCreates = boolCount(first.PinnedWorkspaceCreated)
	rep.FirstRunWorkspaceBytes, err = diskBytes(first.Path)
	if err != nil {
		_ = firstLease.Release()
		return err
	}
	deepest, err := deepestRelativePath(first.Path)
	if err != nil {
		_ = firstLease.Release()
		return err
	}
	rep.DeepestRelativePathChars = len(deepest)
	rep.PathBudgetAvailableChars = opts.maxPath - len(first.Path) - 1 - opts.buildAllowance
	cachePath := filepath.Join(first.Path, ".goobers-build-cache", "state.bin")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		_ = firstLease.Release()
		return err
	}
	cache, err := os.Create(cachePath)
	if err != nil {
		_ = firstLease.Release()
		return err
	}
	if err := cache.Truncate(opts.buildStateBytes); err != nil {
		_ = cache.Close()
		_ = firstLease.Release()
		return err
	}
	if err := cache.Close(); err != nil {
		_ = firstLease.Release()
		return err
	}
	if err := firstLease.Release(); err != nil {
		return err
	}

	start = time.Now()
	secondLease, err := manager.AcquirePinned(ctx, worktree.PinnedOptions{
		RepoURL: repoURL, RunID: "bench-second", BaseRef: opts.baseRef, Branch: "goobers/bench/pinned",
	})
	if err != nil {
		return fmt.Errorf("acquire second pinned lease: %w", err)
	}
	second := secondLease.Worktree
	rep.SecondRunMs = time.Since(start).Milliseconds()
	rep.SecondRunDeltaMs = rep.SecondRunMs - rep.InitToFirstRunMs
	rep.SecondRunWorkspaceCreates = boolCount(second.PinnedWorkspaceCreated)
	rep.SecondRunCreateDelta = rep.SecondRunWorkspaceCreates - rep.FirstRunWorkspaceCreates
	if info, statErr := os.Stat(cachePath); statErr == nil && info.Size() == opts.buildStateBytes {
		rep.BuildStatePreserved = true
	}
	rep.SecondRunWorkspaceBytes, err = diskBytes(second.Path)
	if err == nil {
		rep.SteadyStateBytes, err = diskBytes(manager.PinnedRoot())
	}
	releaseErr := secondLease.Release()
	if err != nil {
		return err
	}
	if releaseErr != nil {
		return releaseErr
	}
	_, _ = fmt.Fprintf(progress, "benchworkcopy: pinned first %dms; second %dms; steady disk %s; deepest path %d/%d chars\n",
		rep.InitToFirstRunMs, rep.SecondRunMs, humanBytes(rep.SteadyStateBytes),
		rep.DeepestRelativePathChars, rep.PathBudgetAvailableChars)
	return nil
}

func enforceLargeRepoGates(rep *report) error {
	if rep.FirstRunWorkspaceBytes < largeRepoWorkingTreeFloor {
		return fmt.Errorf("large-repo gate: working tree %s is below 10 GiB floor", humanBytes(rep.FirstRunWorkspaceBytes))
	}
	if rep.Fixture == nil || rep.Fixture.PathDepth < largeRepoPathDepthFloor {
		return fmt.Errorf("large-repo gate: path depth is below %d", largeRepoPathDepthFloor)
	}
	if rep.DeepestRelativePathChars > rep.PathBudgetAvailableChars {
		return fmt.Errorf("large-repo gate: deepest path %d exceeds LR-5 budget %d", rep.DeepestRelativePathChars, rep.PathBudgetAvailableChars)
	}
	if rep.SteadyStateBytes > largeRepoDiskCeiling {
		return fmt.Errorf("large-repo gate: steady disk %s exceeds %s ceiling", humanBytes(rep.SteadyStateBytes), humanBytes(largeRepoDiskCeiling))
	}
	if rep.FirstRunWorkspaceCreates != 1 || rep.SecondRunWorkspaceCreates != 0 || rep.SecondRunCreateDelta != -1 || !rep.BuildStatePreserved {
		return fmt.Errorf("large-repo gate: second run rematerialized the workspace or lost warm build state")
	}
	return nil
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func deepestRelativePath(root string) (string, error) {
	deepest := ""
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || strings.Contains(path, string(filepath.Separator)+".git") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if len(relative) > len(deepest) {
			deepest = relative
		}
		return nil
	})
	return deepest, err
}

func median(cycles []cycleResult, value func(cycleResult) int64) int64 {
	values := make([]int64, len(cycles))
	for i, c := range cycles {
		values[i] = value(c)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[len(values)/2]
}

func gitVersion(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// fileURL renders path as a file:// URL. The scheme matters: git treats a
// plain local path as a --local clone (object hardlinks, partial-clone
// filters ignored), while file:// exercises the real packfile transport the
// production remotes use.
func fileURL(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p, nil
}

// diskBytes sums apparent file sizes below root without following symlinks —
// the same measurement semantics internal/worktree's usage telemetry reports.
func diskBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
