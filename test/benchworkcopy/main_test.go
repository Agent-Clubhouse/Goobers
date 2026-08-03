package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

// fixtureRefs returns `git for-each-ref` output for the repo at dir — the
// content-hash surface the determinism test compares (every ref name and the
// object id it points at).
func fixtureRefs(ctx context.Context, dir string) (string, error) {
	cmd := testgit.CommandContext(ctx, "-c", "safe.bareRepository=all", "for-each-ref", "--format=%(refname) %(objectname)")
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

// TestRunEndToEndSparse exercises the harness in the mode #649's before/after
// numbers are collected in: a cone-mode worktree must come out smaller than
// a full-checkout worktree of the same fixture.
func TestRunEndToEndSparse(t *testing.T) {
	fullOut := filepath.Join(t.TempDir(), "full.json")
	sparseOut := filepath.Join(t.TempDir(), "sparse.json")
	fixtureDir := filepath.Join(t.TempDir(), "fixture.git")
	baseArgs := []string{
		"-files", "24", "-depth", "4", "-branches", "2", "-tags", "1",
		"-large-blobs", "1", "-large-blob-bytes", "32768",
		"-cycles", "1", "-fixture", fixtureDir, "-keep-fixture",
	}
	var stdout, stderr bytes.Buffer
	if code := run(append(append([]string{}, baseArgs...), "-out", fullOut), &stdout, &stderr); code != 0 {
		t.Fatalf("full run = %d, stderr:\n%s", code, stderr.String())
	}
	if code := run(append(append([]string{}, baseArgs...), "-sparse", "dir000", "-out", sparseOut), &stdout, &stderr); code != 0 {
		t.Fatalf("sparse run = %d, stderr:\n%s", code, stderr.String())
	}

	full := readReport(t, fullOut)
	sparse := readReport(t, sparseOut)
	if len(sparse.Sparse) != 1 || sparse.Sparse[0] != "dir000" {
		t.Fatalf("report does not record sparse: %+v", sparse.Sparse)
	}
	if len(full.Cycles) != 1 || len(sparse.Cycles) != 1 {
		t.Fatalf("cycles = full:%+v sparse:%+v, want one each", full.Cycles, sparse.Cycles)
	}
	if sparse.Cycles[0].WorktreeBytes >= full.Cycles[0].WorktreeBytes {
		t.Fatalf("sparse worktree (%d bytes) is not smaller than the full checkout (%d bytes) — the cone did not apply",
			sparse.Cycles[0].WorktreeBytes, full.Cycles[0].WorktreeBytes)
	}
}

func readReport(t *testing.T, path string) report {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, raw)
	}
	return rep
}

func TestRunRejectsUnknownPreset(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-preset", "galactic"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run = %d, want usage failure 2", code)
	}
}

func TestFixturePathHasConfiguredDepthAndLanguageMix(t *testing.T) {
	for index, extension := range []string{".cpp", ".h", ".cs"} {
		path := fixturePath(index, 12)
		if got := strings.Count(path, "/"); got != 13 {
			t.Fatalf("fixturePath(%d) slash count = %d, want 13: %s", index, got, path)
		}
		if !strings.HasSuffix(path, extension) {
			t.Fatalf("fixturePath(%d) = %q, want %s extension", index, path, extension)
		}
	}
}

func TestPinnedBenchmarkReusesWorkspaceAndPreservesBuildState(t *testing.T) {
	spec := fixtureSpec{
		Seed: 1, Files: 24, HistoryDepth: 2, Branches: 1, Tags: 1,
		PathDepth: 4, SharedBlobs: 3, SharedBlobBytes: 1024,
	}
	var progress bytes.Buffer
	rep, err := benchmark(context.Background(), benchOptions{
		spec: spec, preset: "test", mode: "pinned", baseRef: "main",
		cycles: 1, maxPath: 1024, buildAllowance: 16, buildStateBytes: 4096,
	}, &progress)
	if err != nil {
		t.Fatalf("benchmark: %v", err)
	}
	if rep.FirstRunWorkspaceCreates != 1 || rep.SecondRunWorkspaceCreates != 0 || rep.SecondRunCreateDelta != -1 || !rep.BuildStatePreserved {
		t.Fatalf("pinned warm-run evidence = first creates:%d second creates:%d delta:%d state:%v",
			rep.FirstRunWorkspaceCreates, rep.SecondRunWorkspaceCreates, rep.SecondRunCreateDelta, rep.BuildStatePreserved)
	}
	if rep.SecondRunWorkspaceBytes < rep.FirstRunWorkspaceBytes {
		t.Fatalf("second workspace bytes = %d, first = %d", rep.SecondRunWorkspaceBytes, rep.FirstRunWorkspaceBytes)
	}
	if rep.DeepestRelativePathChars == 0 || rep.DeepestRelativePathChars > rep.PathBudgetAvailableChars {
		t.Fatalf("deepest path/budget = %d/%d", rep.DeepestRelativePathChars, rep.PathBudgetAvailableChars)
	}
}

func TestLargeRepoPresetPinsAcceptanceFloors(t *testing.T) {
	spec := presets["large-repo"]
	if got := int64(spec.Files) * spec.SharedBlobBytes; got < largeRepoWorkingTreeFloor {
		t.Fatalf("large-repo tracked source bytes = %d, want at least %d", got, largeRepoWorkingTreeFloor)
	}
	if spec.PathDepth < largeRepoPathDepthFloor {
		t.Fatalf("large-repo path depth = %d, want at least %d", spec.PathDepth, largeRepoPathDepthFloor)
	}
}

func TestLargeRepoGatesRejectRegressions(t *testing.T) {
	passing := report{
		Fixture:                  &fixtureReport{PathDepth: largeRepoPathDepthFloor},
		FirstRunWorkspaceBytes:   largeRepoWorkingTreeFloor,
		SteadyStateBytes:         largeRepoDiskCeiling,
		DeepestRelativePathChars: 200,
		PathBudgetAvailableChars: 200,
		FirstRunWorkspaceCreates: 1,
		SecondRunCreateDelta:     -1,
		BuildStatePreserved:      true,
	}
	if err := enforceLargeRepoGates(&passing); err != nil {
		t.Fatalf("passing report rejected: %v", err)
	}
	tests := map[string]func(*report){
		"working tree":  func(rep *report) { rep.FirstRunWorkspaceBytes-- },
		"path depth":    func(rep *report) { rep.Fixture.PathDepth-- },
		"path budget":   func(rep *report) { rep.DeepestRelativePathChars++ },
		"steady disk":   func(rep *report) { rep.SteadyStateBytes++ },
		"cold creation": func(rep *report) { rep.FirstRunWorkspaceCreates = 0 },
		"warm creation": func(rep *report) { rep.SecondRunWorkspaceCreates = 1 },
		"warm delta":    func(rep *report) { rep.SecondRunCreateDelta = 0 },
	}
	for name, regress := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := passing
			fixture := *passing.Fixture
			candidate.Fixture = &fixture
			regress(&candidate)
			if err := enforceLargeRepoGates(&candidate); err == nil {
				t.Fatal("regressed report passed")
			}
		})
	}
}
