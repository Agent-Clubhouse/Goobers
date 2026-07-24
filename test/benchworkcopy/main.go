// Command benchworkcopy measures working-copy provisioning cost against a
// deterministic synthetic repo fixture (#641 — B0 of docs/design/v2-cloud-scale.md
// §3). It drives the real internal/worktree Manager API — mirror clone, refresh
// fetch, worktree add, teardown — so every B1–B5 provisioning change is measured
// through the exact code path it modifies, and emits machine-readable JSON so
// runs can be diffed and trended.
//
// Usage:
//
//	go run ./test/benchworkcopy [-preset small|medium|large] [flags]
//	make bench-workcopy                       # medium preset (~1 min end to end)
//
// The generated fixture is a bare repo parameterized by file count, history
// depth, branch/tag count, and blob-size distribution (compressible text plus
// incompressible assets/ binaries), deterministic for a given seed. The
// benchmark clones it over its file:// URL so both modes measure real packfile
// transport (a plain-path clone would hardlink objects and ignore partial-clone
// filters). The "large" preset is deliberately a SPARSE rendition of the
// 10GB/million-file monorepo the design targets: it keeps the file-count and
// history shape (100k files, 300 commits) while sampling large binaries
// sparsely (48 × 4MiB) so default generation survives a CI-class machine. For
// a true multi-GB fixture, crank the binary knobs — generation time and disk
// scale linearly with total blob bytes:
//
//	go run ./test/benchworkcopy -preset large -large-blobs 512 -large-blob-bytes 8388608
//
// JSON output schema ("goobers.bench-workcopy/v1", one object per run):
//
//	schema         string  schema identifier, bumped on incompatible change
//	goos, goarch   string  host platform
//	gitVersion     string  `git version` output
//	partialClone   bool    mirrors provisioned with blobless partial clone (#646)
//	repoURL        string  benchmarked repo (fixture file:// URL or -repo)
//	fixture        object  generation parameters + generateMs + repoBytes
//	                       (omitted when -repo names an existing repo)
//	coldCloneMs    int     first Manager.WorkingCopy call (mirror clone)
//	mirrorBytes    int     mirror disk bytes after the cold clone
//	warmFetchMs    int     second WorkingCopy call (refresh fetch, no changes)
//	cycles         array   per-cycle {worktreeAddMs, worktreeBytes, teardownMs}
//	worktreeAddMsMedian, teardownMsMedian
//	                int     medians across cycles
//
// This tool is invoked explicitly (make bench-workcopy); it is not part of
// `make ci`, and no production package imports it.
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
	Preset         string `json:"preset,omitempty"`
	Seed           int64  `json:"seed"`
	Files          int    `json:"files"`
	HistoryDepth   int    `json:"historyDepth"`
	Branches       int    `json:"branches"`
	Tags           int    `json:"tags"`
	LargeBlobs     int    `json:"largeBlobs"`
	LargeBlobBytes int64  `json:"largeBlobBytes"`
	GenerateMs     int64  `json:"generateMs"`
	RepoBytes      int64  `json:"repoBytes"`
}

type report struct {
	Schema              string         `json:"schema"`
	GOOS                string         `json:"goos"`
	GOARCH              string         `json:"goarch"`
	GitVersion          string         `json:"gitVersion"`
	PartialClone        bool           `json:"partialClone"`
	RepoURL             string         `json:"repoURL"`
	Fixture             *fixtureReport `json:"fixture,omitempty"`
	ColdCloneMs         int64          `json:"coldCloneMs"`
	MirrorBytes         int64          `json:"mirrorBytes"`
	WarmFetchMs         int64          `json:"warmFetchMs"`
	Cycles              []cycleResult  `json:"cycles"`
	WorktreeAddMsMedian int64          `json:"worktreeAddMsMedian"`
	TeardownMsMedian    int64          `json:"teardownMsMedian"`
}

const schemaID = "goobers.bench-workcopy/v1"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("benchworkcopy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	preset := fs.String("preset", "small", "fixture preset: small, medium, or large")
	seed := fs.Int64("seed", 1, "fixture PRNG seed (identical seed+parameters => identical repo)")
	files := fs.Int("files", 0, "override fixture file count")
	depth := fs.Int("depth", 0, "override fixture history depth (commit count)")
	branches := fs.Int("branches", -1, "override fixture branch count")
	tags := fs.Int("tags", -1, "override fixture tag count")
	largeBlobs := fs.Int("large-blobs", -1, "override count of incompressible binaries in the fixture")
	largeBlobBytes := fs.Int64("large-blob-bytes", 0, "override the size of each incompressible binary")
	touch := fs.Int("touch-per-commit", 0, "override files rewritten per history commit")
	cycles := fs.Int("cycles", 3, "worktree add/teardown cycles to measure")
	partialClone := fs.Bool("partial-clone", false, "provision mirrors as blobless partial clones (#646) for before/after comparison")
	fixtureDir := fs.String("fixture", "", "generate (or reuse, if it already exists) the fixture at this path instead of a temp dir")
	keepFixture := fs.Bool("keep-fixture", false, "keep the generated fixture instead of deleting it")
	repo := fs.String("repo", "", "benchmark this existing repo URL instead of generating a fixture")
	baseRef := fs.String("base-ref", "main", "base ref worktrees are provisioned from")
	out := fs.String("out", "", "write the JSON report to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "benchworkcopy: unexpected positional arguments")
		fs.Usage()
		return 2
	}

	spec, ok := presets[*preset]
	if !ok {
		fmt.Fprintf(stderr, "benchworkcopy: unknown preset %q (small, medium, large)\n", *preset)
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
	if *cycles < 1 {
		fmt.Fprintln(stderr, "benchworkcopy: -cycles must be at least 1")
		return 2
	}

	rep, err := benchmark(context.Background(), benchOptions{
		spec:         spec,
		preset:       *preset,
		repo:         *repo,
		fixtureDir:   *fixtureDir,
		keepFixture:  *keepFixture,
		partialClone: *partialClone,
		baseRef:      *baseRef,
		cycles:       *cycles,
	}, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "benchworkcopy: %v\n", err)
		return 1
	}

	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "benchworkcopy: encode report: %v\n", err)
		return 1
	}
	encoded = append(encoded, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, encoded, 0o644); err != nil {
			fmt.Fprintf(stderr, "benchworkcopy: %v\n", err)
			return 1
		}
	} else if _, err := stdout.Write(encoded); err != nil {
		fmt.Fprintf(stderr, "benchworkcopy: %v\n", err)
		return 1
	}
	return 0
}

type benchOptions struct {
	spec         fixtureSpec
	preset       string
	repo         string
	fixtureDir   string
	keepFixture  bool
	partialClone bool
	baseRef      string
	cycles       int
}

func benchmark(ctx context.Context, opts benchOptions, progress io.Writer) (*report, error) {
	rep := &report{
		Schema:       schemaID,
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		GitVersion:   gitVersion(ctx),
		PartialClone: opts.partialClone,
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
				defer os.RemoveAll(tmp)
			}
		}
		fx := &fixtureReport{
			Preset: opts.preset, Seed: opts.spec.Seed,
			Files: opts.spec.Files, HistoryDepth: opts.spec.HistoryDepth,
			Branches: opts.spec.Branches, Tags: opts.spec.Tags,
			LargeBlobs: opts.spec.LargeBlobs, LargeBlobBytes: opts.spec.LargeBlobBytes,
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
		fmt.Fprintf(progress, "benchworkcopy: fixture %s (%d files, depth %d) ready in %dms (%s)\n",
			opts.preset, opts.spec.Files, opts.spec.HistoryDepth, fx.GenerateMs, humanBytes(fx.RepoBytes))
		if opts.keepFixture {
			fmt.Fprintf(progress, "benchworkcopy: fixture kept at %s\n", dir)
		}
	}
	rep.RepoURL = repoURL

	workRoot, err := os.MkdirTemp("", "goobers-bench-workcopies-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workRoot)

	var managerOpts []worktree.ManagerOption
	if opts.partialClone {
		managerOpts = append(managerOpts, worktree.WithPartialClone())
	}
	manager, err := worktree.NewManager(workRoot, managerOpts...)
	if err != nil {
		return nil, err
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
	fmt.Fprintf(progress, "benchworkcopy: cold clone %dms (mirror %s); warm fetch %dms\n",
		rep.ColdCloneMs, humanBytes(rep.MirrorBytes), rep.WarmFetchMs)

	for i := 0; i < opts.cycles; i++ {
		var cycle cycleResult
		start = time.Now()
		wt, err := manager.Create(ctx, worktree.CreateOptions{
			RepoURL: repoURL,
			RunID:   fmt.Sprintf("bench-%d", i),
			BaseRef: opts.baseRef,
			Branch:  fmt.Sprintf("goobers/bench/run%d", i),
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
	fmt.Fprintf(progress, "benchworkcopy: worktree add median %dms; teardown median %dms (%d cycles)\n",
		rep.WorktreeAddMsMedian, rep.TeardownMsMedian, opts.cycles)
	return rep, nil
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
