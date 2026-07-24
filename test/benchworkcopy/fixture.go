package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os/exec"
	"strings"
)

// fixtureSpec parameterizes the synthetic bare-repo fixture. Identical specs
// (including Seed) produce byte-identical repositories: authorship and
// timestamps are fixed, content and structure derive from a seeded PRNG, and
// the whole repo is materialized through a single deterministic
// `git fast-import` stream.
type fixtureSpec struct {
	Seed int64
	// Files is the number of text files present in every commit's tree.
	Files int
	// HistoryDepth is the number of commits on main; each commit after the
	// first rewrites TouchPerCommit files.
	HistoryDepth int
	// Branches is the number of side branches, each one commit off a
	// deterministic point on main.
	Branches int
	// Tags is the number of lightweight tags on deterministic main commits.
	Tags int
	// LargeBlobs incompressible binaries of LargeBlobBytes each are committed
	// under assets/ in the first commit — the knob that scales the fixture
	// toward the multi-GB monorepo shape (see the package doc comment).
	LargeBlobs     int
	LargeBlobBytes int64
	// TouchPerCommit is the number of files each history commit rewrites.
	// Zero derives max(1, Files/HistoryDepth) capped at 500.
	TouchPerCommit int
}

// presets are the built-in fixture sizes. "large" keeps the file-count and
// history shape of the monorepo the design targets while sampling large
// binaries sparsely so default generation stays CI-survivable; crank
// -large-blobs/-large-blob-bytes for a true multi-GB fixture.
var presets = map[string]fixtureSpec{
	"small": {
		Files: 250, HistoryDepth: 25, Branches: 4, Tags: 2,
		LargeBlobs: 4, LargeBlobBytes: 256 << 10,
	},
	"medium": {
		Files: 5000, HistoryDepth: 120, Branches: 16, Tags: 4,
		LargeBlobs: 16, LargeBlobBytes: 1 << 20,
	},
	"large": {
		Files: 100000, HistoryDepth: 300, Branches: 32, Tags: 8,
		LargeBlobs: 48, LargeBlobBytes: 4 << 20,
	},
}

func (s fixtureSpec) touchPerCommit() int {
	if s.TouchPerCommit > 0 {
		return s.TouchPerCommit
	}
	touch := s.Files / s.HistoryDepth
	if touch < 1 {
		touch = 1
	}
	if touch > 500 {
		touch = 500
	}
	return touch
}

// generateFixture materializes spec as a bare repository at dir. The repo is
// created with uploadpack.allowfilter enabled so partial-clone benchmarking
// works over its file:// URL (plain-path clones bypass the filter entirely).
func generateFixture(ctx context.Context, spec fixtureSpec, dir string) error {
	if spec.Files < 1 || spec.HistoryDepth < 1 {
		return fmt.Errorf("fixture needs at least one file and one commit (files=%d, depth=%d)", spec.Files, spec.HistoryDepth)
	}
	if err := runFixtureGit(ctx, "", "init", "--bare", "--quiet", dir); err != nil {
		return err
	}
	// Pin HEAD so the fixture is independent of the host's init.defaultBranch.
	if err := runFixtureGit(ctx, dir, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	if err := runFixtureGit(ctx, dir, "config", "uploadpack.allowfilter", "true"); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "fast-import", "--quiet")
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("fast-import stdin: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git fast-import: %w", err)
	}
	streamErr := writeImportStream(stdin, spec)
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("git fast-import: %v: %s", waitErr, stderr.Bytes())
	}
	if streamErr != nil {
		return fmt.Errorf("generate fast-import stream: %w", streamErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close fast-import stream: %w", closeErr)
	}
	return nil
}

func runFixtureGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %v: %s", args, err, out)
	}
	return nil
}

// fixedEpoch anchors every commit timestamp so identical specs hash to
// identical commits regardless of when or where the fixture is generated.
const fixedEpoch = 1700000000

const fixtureIdent = "Goobers Bench <bench@goobers.invalid>"

func writeImportStream(w io.Writer, spec fixtureSpec) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	rng := rand.New(rand.NewSource(spec.Seed))
	paths := make([]string, spec.Files)
	for i := range paths {
		paths[i] = fmt.Sprintf("dir%03d/sub%02d/file%06d.txt", i%97, (i/97)%13, i)
	}

	mark := 0
	emitCommit := func(ref string, seq int, from int, msg string) (int, error) {
		mark++
		if _, err := fmt.Fprintf(bw, "commit %s\nmark :%d\n", ref, mark); err != nil {
			return 0, err
		}
		when := fixedEpoch + seq
		if _, err := fmt.Fprintf(bw, "author %s %d +0000\ncommitter %s %d +0000\n", fixtureIdent, when, fixtureIdent, when); err != nil {
			return 0, err
		}
		if _, err := fmt.Fprintf(bw, "data %d\n%s\n", len(msg), msg); err != nil {
			return 0, err
		}
		if from > 0 {
			if _, err := fmt.Fprintf(bw, "from :%d\n", from); err != nil {
				return 0, err
			}
		}
		return mark, nil
	}
	modifyText := func(path string, rng *rand.Rand) error {
		content := textBlob(rng)
		if _, err := fmt.Fprintf(bw, "M 100644 inline %s\ndata %d\n", path, len(content)); err != nil {
			return err
		}
		if _, err := bw.Write(content); err != nil {
			return err
		}
		return bw.WriteByte('\n')
	}

	// Root commit: the full text tree plus the incompressible binaries.
	if _, err := emitCommit("refs/heads/main", 0, 0, "bench: seed tree"); err != nil {
		return err
	}
	for _, path := range paths {
		if err := modifyText(path, rng); err != nil {
			return err
		}
	}
	chunk := make([]byte, 1<<20)
	for b := 0; b < spec.LargeBlobs; b++ {
		if _, err := fmt.Fprintf(bw, "M 100644 inline assets/blob%04d.bin\ndata %d\n", b, spec.LargeBlobBytes); err != nil {
			return err
		}
		remaining := spec.LargeBlobBytes
		for remaining > 0 {
			n := int64(len(chunk))
			if remaining < n {
				n = remaining
			}
			rng.Read(chunk[:n])
			if _, err := bw.Write(chunk[:n]); err != nil {
				return err
			}
			remaining -= n
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}

	// History: each commit rewrites a deterministic subset of the tree.
	mainMarks := make([]int, 0, spec.HistoryDepth)
	mainMarks = append(mainMarks, mark)
	touch := spec.touchPerCommit()
	for c := 1; c < spec.HistoryDepth; c++ {
		if _, err := emitCommit("refs/heads/main", c, 0, fmt.Sprintf("bench: history %d", c)); err != nil {
			return err
		}
		for t := 0; t < touch; t++ {
			if err := modifyText(paths[rng.Intn(len(paths))], rng); err != nil {
				return err
			}
		}
		mainMarks = append(mainMarks, mark)
	}

	// Side branches, each one commit off a deterministic point on main.
	for b := 0; b < spec.Branches; b++ {
		base := mainMarks[rng.Intn(len(mainMarks))]
		ref := fmt.Sprintf("refs/heads/bench/branch%03d", b)
		if _, err := emitCommit(ref, spec.HistoryDepth+b, base, fmt.Sprintf("bench: branch %d", b)); err != nil {
			return err
		}
		if err := modifyText(paths[rng.Intn(len(paths))], rng); err != nil {
			return err
		}
	}

	// Lightweight tags on deterministic main commits.
	for t := 0; t < spec.Tags; t++ {
		base := mainMarks[rng.Intn(len(mainMarks))]
		if _, err := fmt.Fprintf(bw, "reset refs/tags/bench-tag%02d\nfrom :%d\n", t, base); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// textBlob returns pseudo-source text with a bucketed size distribution
// (mostly sub-KiB files, a tail of multi-KiB ones) — compressible the way
// real source trees are, unlike the assets/ binaries.
func textBlob(rng *rand.Rand) []byte {
	var size int
	switch bucket := rng.Intn(10); {
	case bucket < 6:
		size = 200 + rng.Intn(800)
	case bucket < 9:
		size = 1024 + rng.Intn(3072)
	default:
		size = 8192 + rng.Intn(24576)
	}
	var b strings.Builder
	b.Grow(size + 64)
	for b.Len() < size {
		fmt.Fprintf(&b, "line %08x %08x %08x %08x\n", rng.Uint32(), rng.Uint32(), rng.Uint32(), rng.Uint32())
	}
	return []byte(b.String())
}

// fixtureRefs returns `git for-each-ref` output for the repo at dir — the
// content-hash surface the determinism test compares (every ref name and the
// object id it points at).
func fixtureRefs(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(refname) %(objectname)")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git for-each-ref: %w", err)
	}
	return string(out), nil
}
