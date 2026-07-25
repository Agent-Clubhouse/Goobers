package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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

// tinySpec keeps generator tests fast: the point is shape and determinism,
// not scale.
var tinySpec = fixtureSpec{
	Seed:           7,
	Files:          24,
	HistoryDepth:   4,
	Branches:       2,
	Tags:           1,
	LargeBlobs:     1,
	LargeBlobBytes: 32 << 10,
	TouchPerCommit: 4,
}

func TestGenerateFixtureDeterministic(t *testing.T) {
	ctx := context.Background()
	first := filepath.Join(t.TempDir(), "a.git")
	second := filepath.Join(t.TempDir(), "b.git")
	if err := generateFixture(ctx, tinySpec, first); err != nil {
		t.Fatalf("generate first fixture: %v", err)
	}
	if err := generateFixture(ctx, tinySpec, second); err != nil {
		t.Fatalf("generate second fixture: %v", err)
	}

	firstRefs, err := fixtureRefs(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	secondRefs, err := fixtureRefs(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if firstRefs == "" {
		t.Fatal("fixture has no refs")
	}
	if firstRefs != secondRefs {
		t.Fatalf("identical specs produced different repos:\nfirst:\n%s\nsecond:\n%s", firstRefs, secondRefs)
	}

	changed := tinySpec
	changed.Seed = 8
	third := filepath.Join(t.TempDir(), "c.git")
	if err := generateFixture(ctx, changed, third); err != nil {
		t.Fatalf("generate reseeded fixture: %v", err)
	}
	thirdRefs, err := fixtureRefs(ctx, third)
	if err != nil {
		t.Fatal(err)
	}
	if thirdRefs == firstRefs {
		t.Fatal("different seeds produced identical repos — content is not seed-derived")
	}
}

func TestRunEndToEnd(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	args := []string{
		"-files", "24", "-depth", "4", "-branches", "2", "-tags", "1",
		"-large-blobs", "1", "-large-blob-bytes", "32768",
		"-cycles", "2", "-out", out,
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr:\n%s", code, stderr.String())
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, raw)
	}
	if rep.Schema != schemaID {
		t.Fatalf("schema = %q, want %q", rep.Schema, schemaID)
	}
	if rep.Fixture == nil || rep.Fixture.Files != 24 || rep.Fixture.HistoryDepth != 4 {
		t.Fatalf("fixture report = %+v, want files=24 depth=4", rep.Fixture)
	}
	if rep.MirrorBytes <= 0 {
		t.Fatalf("mirrorBytes = %d, want > 0", rep.MirrorBytes)
	}
	if len(rep.Cycles) != 2 {
		t.Fatalf("cycles = %d, want 2", len(rep.Cycles))
	}
	for i, cycle := range rep.Cycles {
		if cycle.WorktreeBytes <= 0 {
			t.Fatalf("cycle %d worktreeBytes = %d, want > 0", i, cycle.WorktreeBytes)
		}
	}
}

// TestRunEndToEndPartialClone exercises the harness in the mode B1 (#646)
// before/after numbers are collected in: a blobless mirror must come out
// smaller than the fixture while still provisioning full worktrees.
func TestRunEndToEndPartialClone(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	args := []string{
		"-files", "24", "-depth", "4", "-branches", "2", "-tags", "1",
		"-large-blobs", "1", "-large-blob-bytes", "32768",
		"-cycles", "1", "-partial-clone", "-out", out,
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr:\n%s", code, stderr.String())
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, raw)
	}
	if !rep.PartialClone {
		t.Fatal("report does not record partialClone: true")
	}
	if rep.MirrorBytes >= rep.Fixture.RepoBytes {
		t.Fatalf("blobless mirror (%d bytes) is not smaller than the fixture (%d bytes) — the filter did not apply", rep.MirrorBytes, rep.Fixture.RepoBytes)
	}
	if len(rep.Cycles) != 1 || rep.Cycles[0].WorktreeBytes <= 0 {
		t.Fatalf("cycles = %+v, want one cycle with a materialized worktree", rep.Cycles)
	}
}

func TestRunRejectsUnknownPreset(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-preset", "galactic"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run = %d, want usage failure 2", code)
	}
}
