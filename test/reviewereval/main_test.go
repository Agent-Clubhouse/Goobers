package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/testgit"
)

// fixtureRepo is a small history with known bugs, fixes, and noise.
type fixtureRepo struct {
	dir     string
	commits map[string]string
	step    int
}

func (repo *fixtureRepo) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := testgit.Command(append([]string{"-C", repo.dir, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com"}, args...)...)
	date := fmt.Sprintf("2024-01-%02dT12:00:00Z", repo.step+1)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files (an empty content deletes the file) and commits them
// under name.
func (repo *fixtureRepo) commit(t *testing.T, name, message string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		target := filepath.Join(repo.dir, path)
		if content == "" {
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo.step++
	repo.git(t, "add", "-A")
	repo.git(t, "commit", "-q", "-m", message)
	repo.commits[name] = repo.git(t, "rev-parse", "HEAD")
}

const (
	rootA = "package a\n\nvar shared = 7\n\nfunc A() int {\n\tbase := 1\n\n\tfirst := base + 10\n\treturn first\n}\n"
	// introA's line 9 is the bug fix1 later corrects.
	introA = "package a\n\nvar shared = 7\n\nfunc A() int {\n\tbase := 1\n\n\tfirst := base + 10\n\tsecond := first * 3\n\treturn second\n}\n"
	fixedA = "package a\n\nfunc A() int {\n\tbase := 1\n\tfirst := base+10\n\tsecond := first * 2\n\treturn second\n}\n"
	introE = "package a\n\nfunc E() (int, int) {\n\tx := 1\n\ty := 2\n\treturn x, y\n}\n"
)

// newFixtureRepo builds a history holding one fix of each kind:
//   - fix1 corrects a bug introduced by intro, and also reformats a line,
//     deletes blank lines, moves a declaration to another file, and adds a
//     regression test intro never touched (not out-of-diff: tests are
//     excluded);
//   - fix2 corrects a line from the root commit (no culprit to name);
//   - fix3 only adds lines (nothing to trace);
//   - fix4 corrects intro2 but mostly changes a file intro2 never touched;
//   - fix5 corrects one line from each of two commits (a tie: ambiguous);
//   - fix6 corrects intro3's line after a pure reordering refactor.
func newFixtureRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	repo := &fixtureRepo{dir: t.TempDir(), commits: map[string]string{}}
	repo.git(t, "init", "-q", "-b", "main")
	repo.commit(t, "root", "initial", map[string]string{
		"a.go": rootA, "b.go": "package a\n\nfunc B() int {\n\treturn 2\n}\n", "c.go": "package a\n\nfunc C() int {\n\treturn 3\n}\n",
	})
	repo.commit(t, "intro", "feat: scale the total", map[string]string{"a.go": introA})
	repo.commit(t, "docs", "docs: describe A", map[string]string{"README.md": "A scales its total.\n"})
	repo.commit(t, "fix1", "fix: correct the total multiplier\n\nFixes #41", map[string]string{
		"a.go": fixedA, "b.go": "package a\n\nvar shared = 7\n\nfunc B() int {\n\treturn 2\n}\n",
		"a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif A() != 22 {\n\t\tt.Fatal(A())\n\t}\n}\n",
	})
	repo.commit(t, "fix2", "fix: B returns twenty", map[string]string{"b.go": "package a\n\nvar shared = 7\n\nfunc B() int {\n\treturn 20\n}\n"})
	repo.commit(t, "fix3", "fix: add a guard", map[string]string{"c.go": "package a\n\nfunc C() int {\n\treturn 3\n}\n\nfunc guard() {}\n"})
	repo.commit(t, "intro2", "feat: name the count", map[string]string{"c.go": "package a\n\nfunc C() int {\n\tcount := 3\n\treturn count\n}\n\nfunc guard() {}\n"})
	repo.commit(t, "fix4", "fix(c): load the count", map[string]string{
		"c.go":      "package a\n\nfunc C() int {\n\tcount := load()\n\treturn count\n}\n\nfunc guard() {}\n",
		"config.go": "package a\n\nfunc load() int {\n\treturn 4\n}\n",
	})
	repo.commit(t, "d1", "feat: one", map[string]string{"d.go": "package a\n\nvar one = 1\n"})
	repo.commit(t, "d2", "feat: two", map[string]string{"d.go": "package a\n\nvar one = 1\nvar two = 2\n"})
	repo.commit(t, "d3", "feat: three", map[string]string{"d.go": "package a\n\nvar one = 1\nvar two = 2\nvar three = 3\n"})
	repo.commit(t, "fix5", "fix: collapse the values", map[string]string{"d.go": "package a\n\nvar one = 1\nvar all = 5\n"})
	repo.commit(t, "intro3", "feat: E", map[string]string{"e.go": introE})
	repo.commit(t, "refactor", "refactor: declare y first", map[string]string{"e.go": "package a\n\nfunc E() (int, int) {\n\ty := 2\n\tx := 1\n\treturn x, y\n}\n"})
	repo.commit(t, "fix6", "fix: E starts at zero", map[string]string{"e.go": "package a\n\nfunc E() (int, int) {\n\ty := 2\n\tx := 0\n\treturn x, y\n}\n"})
	return repo
}

// liveState is everything about the live repository the eval must not change.
func (repo *fixtureRepo) liveState(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		repo.git(t, "for-each-ref"), repo.git(t, "rev-parse", "HEAD"), repo.git(t, "status", "--porcelain", "--ignored"),
		repo.git(t, "count-objects", "-v"), repo.git(t, "worktree", "list", "--porcelain"),
	}, "\n")
}

func buildFixture(t *testing.T, repo *fixtureRepo) dataset {
	t.Helper()
	set, err := newBuilder(repo.dir).build(context.Background(), buildOptions{
		Range: "main", Version: "fixture-v1", BugIssues: map[int]bool{41: true},
		HandLabels: []handLabel{{FixCommit: repo.commits["fix1"][:10], IntroducingCommit: repo.commits["intro"][:12], LabeledBy: "maintainer", LabeledAt: "2024-02-01"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func caseFor(set dataset, fix string) *datasetCase {
	for index := range set.Cases {
		if set.Cases[index].FixCommit == fix {
			return &set.Cases[index]
		}
	}
	return nil
}

func TestBuildTracesFixesWithRSZZ(t *testing.T) {
	t.Parallel()
	repo := newFixtureRepo(t)
	before := repo.liveState(t)
	set := buildFixture(t, repo)
	if after := repo.liveState(t); after != before {
		t.Fatalf("building the dataset changed the live repository:\nbefore %s\nafter %s", before, after)
	}

	header := set.Header
	if header.FixesFound != 6 || header.ToCommit != repo.commits["fix6"] {
		t.Fatalf("header = %+v, want 6 fixes ending at fix6", header)
	}
	wantDropped := map[string]int{dropRootCommit: 1, dropNoTraceableLines: 1, dropAmbiguous: 1}
	if !reflect.DeepEqual(header.Dropped, wantDropped) {
		t.Fatalf("dropped = %v, want %v", header.Dropped, wantDropped)
	}
	for _, filter := range []string{filterWhitespace, filterFormat, filterMove, filterPureRefactor} {
		if header.Filtered[filter] < 1 {
			t.Errorf("filter %s removed %d lines, want at least 1 (filtered %v)", filter, header.Filtered[filter], header.Filtered)
		}
	}

	fix1 := caseFor(set, repo.commits["fix1"])
	if fix1 == nil || fix1.IntroducingCommit != repo.commits["intro"] || fix1.Confidence != 1 || fix1.FixSource != fixSourceBugIssue ||
		!reflect.DeepEqual(fix1.FixIssues, []int{41}) || fix1.OutOfDiff {
		t.Fatalf("fix1 case = %+v, want intro blamed with full confidence", fix1)
	}
	if want := []fileLines{{Path: "a.go", Lines: []int{9}}}; !reflect.DeepEqual(fix1.BugLines, want) {
		t.Fatalf("fix1 bug lines = %+v, want %+v", fix1.BugLines, want)
	}
	if fix1.HandLabel == nil || fix1.HandLabel.LabeledBy != "maintainer" || fix1.Split != splitOf(repo.commits["intro"]) {
		t.Fatalf("fix1 hand label/split = %+v %s", fix1.HandLabel, fix1.Split)
	}

	fix4 := caseFor(set, repo.commits["fix4"])
	if fix4 == nil || fix4.IntroducingCommit != repo.commits["intro2"] || !fix4.OutOfDiff || !containsString(fix4.Flags, flagOutOfDiff) {
		t.Fatalf("fix4 case = %+v, want intro2 tagged out-of-diff", fix4)
	}
	fix6 := caseFor(set, repo.commits["fix6"])
	if fix6 == nil || fix6.IntroducingCommit != repo.commits["intro3"] {
		t.Fatalf("fix6 case = %+v, want the refactor skipped and intro3 blamed", fix6)
	}

	var first, second bytes.Buffer
	if err := writeDataset(&first, set); err != nil {
		t.Fatal(err)
	}
	if err := writeDataset(&second, buildFixture(t, repo)); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatal("rebuilding the same range produced a different dataset")
	}
	loaded, err := parseDataset(first.Bytes())
	if err != nil || loaded.Header.Digest != set.Header.Digest || len(loaded.Cases) != len(set.Cases) {
		t.Fatalf("round trip = %+v, %v", loaded.Header, err)
	}
}

func TestGitReaderRefusesWrites(t *testing.T) {
	reader := gitReader{dir: t.TempDir()}
	for _, args := range [][]string{
		{"commit", "-m", "x"}, {"update-ref", "HEAD", "x"}, {"worktree", "add", "x"},
		{"diff", "--output=patch"}, {"log", "-o", "x"}, {},
	} {
		if _, err := reader.run(context.Background(), args...); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("git %v: err = %v, want a refusal", args, err)
		}
	}
}

func TestDatasetRejectsTamperingAndMixedSplits(t *testing.T) {
	split := func(intro string) string { return splitOf(intro) }
	other := func(s string) string {
		if s == splitTrain {
			return splitEval
		}
		return splitTrain
	}
	item := func(id, fix, intro string) datasetCase {
		return datasetCase{
			ID: id, Split: split(intro), FixCommit: fix, IntroducingCommit: intro, Confidence: 1,
			Candidates: []candidate{{Commit: intro, Lines: 1, Confidence: 1}}, BugLines: []fileLines{{Path: "a.go", Lines: []int{1}}},
		}
	}
	valid := dataset{Header: datasetHeader{Schema: datasetSchemaVersion, Version: "v1"}, Cases: []datasetCase{item("one", "f1", "i1"), item("two", "f2", "i2")}}
	encode := func(set dataset) []byte {
		var out bytes.Buffer
		if err := writeDataset(&out, set); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	if _, err := parseDataset(encode(valid)); err != nil {
		t.Fatalf("valid dataset: %v", err)
	}
	tampered := bytes.Replace(encode(valid), []byte(`"fixCommit":"f1"`), []byte(`"fixCommit":"f9"`), 1)
	if _, err := parseDataset(tampered); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("tampered dataset err = %v, want a digest mismatch", err)
	}

	misplaced := item("one", "f1", "i1")
	misplaced.Split = other(misplaced.Split)
	repeatedFix := item("three", "f1", "i3")
	for name, cases := range map[string][]datasetCase{
		"split off the rule": {misplaced},
		"fix in two cases":   {item("one", "f1", "i1"), repeatedFix},
	} {
		if _, err := parseDataset(encode(dataset{Header: valid.Header, Cases: cases})); err == nil {
			t.Errorf("%s: dataset accepted", name)
		}
	}
}

// fakeReviewer records each request and checks the replayed repository is
// isolated before answering.
type fakeReviewer struct {
	t        *testing.T
	repo     *fixtureRepo
	requests []reviewRequest
	answer   func(reviewRequest) reviewResult
}

func (fake *fakeReviewer) Review(_ context.Context, request reviewRequest) (reviewResult, error) {
	fake.t.Helper()
	fake.requests = append(fake.requests, request)
	inRepo := func(args ...string) (string, error) {
		out, err := testgit.Command(append([]string{"-C", request.Dir}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if count, err := inRepo("rev-list", "--all", "--count"); err != nil || count != "2" {
		fake.t.Errorf("replayed repository holds %q commits (%v), want exactly base and head", count, err)
	}
	for _, later := range []string{"fix1", "fix6"} {
		if _, err := inRepo("cat-file", "-e", fake.repo.commits[later]); err == nil {
			fake.t.Errorf("replayed repository can reach later commit %s", later)
		}
	}
	if remotes, err := inRepo("remote"); err != nil || remotes != "" {
		fake.t.Errorf("replayed repository remotes = %q, %v; want none", remotes, err)
	}
	if head, err := inRepo("rev-parse", "HEAD"); err != nil || head != request.HeadSHA {
		fake.t.Errorf("replayed HEAD = %q, want %s", head, request.HeadSHA)
	}
	return fake.answer(request), nil
}

func replayFixture(t *testing.T) (*fixtureRepo, dataset) {
	t.Helper()
	repo := newFixtureRepo(t)
	label := &handLabel{IntroducingCommit: repo.commits["intro"][:12], LabeledBy: "maintainer", LabeledAt: "2024-02-01"}
	item := func(id, split, fix, intro string, bug fileLines) datasetCase {
		return datasetCase{
			ID: id, Split: split, FixCommit: repo.commits[fix], IntroducingCommit: repo.commits[intro], Confidence: 1,
			Candidates: []candidate{{Commit: repo.commits[intro], Lines: 1, Confidence: 1}}, BugLines: []fileLines{bug},
		}
	}
	set := dataset{Header: datasetHeader{Schema: datasetSchemaVersion, Version: "fixture-v1", Digest: "sha256:fixture"}}
	for _, id := range []string{"case-a", "case-b", "case-c", "case-d", "case-train"} {
		split := splitEval
		if id == "case-train" {
			split = splitTrain
		}
		labeled := item(id, split, "fix1", "intro", fileLines{Path: "a.go", Lines: []int{9}})
		labeled.HandLabel = label
		set.Cases = append(set.Cases, labeled)
	}
	outside := item("case-outside", splitEval, "fix4", "intro2", fileLines{Path: "c.go", Lines: []int{4}})
	outside.OutOfDiff = true
	set.Cases = append(set.Cases, outside)
	return repo, set
}

// failingJudge answers the first finding of each case, then fails.
type failingJudge struct{ calls int }

func (judge *failingJudge) Name() string { return "failing" }

func (judge *failingJudge) Judge(context.Context, judgeRequest) (judgeDecision, error) {
	judge.calls++
	if judge.calls%2 == 0 {
		return judgeDecision{}, fmt.Errorf("judge unavailable")
	}
	return judgeDecision{Rationale: "unrelated", CostUSD: 0.2}, nil
}

type overspendingJudge struct{}

func (overspendingJudge) Name() string { return "overspending" }

func (overspendingJudge) Judge(_ context.Context, request judgeRequest) (judgeDecision, error) {
	return judgeDecision{Rationale: "costly", CostUSD: request.BudgetRemainingUSD + 0.1}, nil
}

type fakeJudge struct{ requests int }

func (judge *fakeJudge) Name() string { return "fake" }

func (judge *fakeJudge) Judge(_ context.Context, request judgeRequest) (judgeDecision, error) {
	judge.requests++
	return judgeDecision{Match: request.Finding.Class == apiv1.FindingMissingTests, Rationale: "names the untested multiplier"}, nil
}

func TestReplayScoresIsolatedReviewsPerClass(t *testing.T) {
	t.Parallel()
	repo, set := replayFixture(t)
	set.Cases = append(set.Cases[:2], set.Cases[4:]...)
	before := repo.liveState(t)
	reviewer := &fakeReviewer{t: t, repo: repo, answer: func(request reviewRequest) reviewResult {
		if !strings.Contains(request.Diff, "+\tsecond := first * 3") || request.Title != "feat: scale the total" {
			t.Errorf("request diff/title = %q %q, want the offending change", request.Diff, request.Title)
		}
		if request.CaseID != "case-a" {
			return reviewResult{Verdict: apiv1.Verdict{Decision: apiv1.VerdictPass}, CostUSD: 0.1}
		}
		return reviewResult{CostUSD: 0.3, Verdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Findings: []apiv1.Finding{
			{Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityError, Location: "a.go:8", Message: "multiplier"},
			{Class: apiv1.FindingMissingTests, Severity: apiv1.SeverityWarning, Location: "a.go", Message: "untested"},
			{Class: apiv1.FindingScopeCreep, Severity: apiv1.SeverityInfo, Location: "a.go:1", Message: "unrelated"},
		}}}
	}}
	judge := &fakeJudge{}
	options := replayOptions{Split: splitEval, Variant: "baseline", MaxCases: 10, BudgetUSD: 5, Seed: "s", WorkDir: t.TempDir()}
	report, err := replay(context.Background(), set, gitReader{dir: repo.dir}, reviewer, matcher{judge: judge}, options)
	if err != nil {
		t.Fatal(err)
	}
	if after := repo.liveState(t); after != before {
		t.Fatal("replay changed the live repository")
	}
	if report.Sampled != 2 || report.Reviewed != 2 || report.OutOfDiff != 1 || len(reviewer.requests) != 2 {
		t.Fatalf("report counts = %+v, want 2 eval cases reviewed and 1 out-of-diff excluded", report)
	}
	for _, request := range reviewer.requests {
		if request.CaseID == "case-train" {
			t.Fatal("replay of the eval split reviewed a training case")
		}
	}
	wantOverall := metricsOf("", 1, 2, 0.5, 2, 3, 0.6667)
	wantClasses := []creditgraph.ReviewerEvalMetrics{
		metricsOf(string(apiv1.FindingMissingTests), 1, 2, 0.5, 1, 1, 1),
		metricsOf(string(apiv1.FindingScopeCreep), 0, 2, 0, 0, 1, 0),
		metricsOf(string(apiv1.FindingSubstantive), 1, 2, 0.5, 1, 1, 1),
	}
	if report.Overall != wantOverall || !reflect.DeepEqual(report.Classes, wantClasses) {
		t.Fatalf("metrics = %+v %+v, want %+v %+v", report.Overall, report.Classes, wantOverall, wantClasses)
	}
	if report.TotalCostUSD != 0.4 || report.CostPerBugUSD != 0.2 {
		t.Fatalf("cost = %v total, %v per bug; want 0.4 and 0.2", report.TotalCostUSD, report.CostPerBugUSD)
	}
	if report.SZZCalibration != (szzCalibration{Labeled: 2, Agreed: 2, Agreement: 1}) {
		t.Fatalf("SZZ calibration = %+v, want both hand-labeled eval cases agreeing", report.SZZCalibration)
	}
	var scored caseResult
	for _, result := range report.Results {
		if result.CaseID == "case-a" {
			scored = result
		}
	}
	if scored.Verdict == nil || len(scored.Verdict.Findings) != 3 || !scored.Caught ||
		scored.Findings[0].Match != matchLineOverlap || scored.Findings[1].Match != matchJudge || scored.Findings[2].Match != "" {
		t.Fatalf("case-a result = %+v, want the verdict recorded and findings matched by overlap and judge", scored)
	}
	if judge.requests != 2 || len(scored.JudgeDecisions) != 2 || !scored.JudgeDecisions[0].Match || scored.JudgeDecisions[1].Match ||
		scored.JudgeDecisions[0].Judge != "fake" {
		t.Fatalf("judge decisions = %+v, want both non-overlapping findings judged and recorded", scored.JudgeDecisions)
	}
	if err := report.summary().Validate(); err != nil {
		t.Fatalf("summary does not validate: %v", err)
	}
}

func metricsOf(class string, caught, evaluated int, recall float64, matched, findings int, precision float64) creditgraph.ReviewerEvalMetrics {
	return creditgraph.ReviewerEvalMetrics{Class: class, Caught: caught, Evaluated: evaluated, Recall: recall, MatchedFindings: matched, Findings: findings, Precision: precision}
}

func TestReplayGuardrails(t *testing.T) {
	t.Parallel()
	repo, set := replayFixture(t)
	answer := func(request reviewRequest) reviewResult {
		return reviewResult{Verdict: apiv1.Verdict{Decision: apiv1.VerdictPass}, CostUSD: min(1, request.BudgetRemainingUSD)}
	}
	run := func(options replayOptions) evalReport {
		t.Helper()
		options.Split, options.Variant, options.WorkDir = splitEval, "baseline", t.TempDir()
		report, err := replay(context.Background(), set, gitReader{dir: repo.dir}, &fakeReviewer{t: t, repo: repo, answer: answer}, matcher{}, options)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	order := func(report evalReport) []string {
		var ids []string
		for _, result := range report.Results {
			ids = append(ids, result.CaseID)
		}
		return ids
	}

	budgeted := run(replayOptions{MaxCases: 10, BudgetUSD: 2.5, Seed: "s"})
	if budgeted.Sampled != 4 || budgeted.Reviewed != 3 || budgeted.BudgetExhausted != 1 || budgeted.TotalCostUSD != 2.5 ||
		budgeted.TotalCostUSD > budgeted.BudgetUSD || budgeted.validate() != nil {
		t.Fatalf("budgeted replay = %+v, want 3 reviewed within the 2.5 cap and the fourth not run", budgeted)
	}
	if again := run(replayOptions{MaxCases: 10, BudgetUSD: 2.5, Seed: "s"}); !reflect.DeepEqual(order(again), order(budgeted)) {
		t.Fatalf("same seed sampled %v then %v", order(budgeted), order(again))
	}
	capped := run(replayOptions{MaxCases: 2, BudgetUSD: 10, Seed: "s"})
	if !reflect.DeepEqual(order(capped), order(budgeted)[:2]) {
		t.Fatalf("max-cases 2 sampled %v, want the first two of %v", order(capped), order(budgeted))
	}

	for _, bad := range []replayOptions{
		{Split: splitEval, Variant: "v", MaxCases: hardMaxCases + 1, BudgetUSD: 1, Seed: "s"},
		{Split: splitEval, Variant: "v", MaxCases: 1, BudgetUSD: 0, Seed: "s"},
		{Split: "all", Variant: "v", MaxCases: 1, BudgetUSD: 1, Seed: "s"},
		{Split: splitEval, Variant: "v", MaxCases: 1, BudgetUSD: 1},
	} {
		if _, err := replay(context.Background(), set, gitReader{dir: repo.dir}, &fakeReviewer{t: t, repo: repo, answer: answer}, matcher{}, bad); err == nil {
			t.Errorf("replay accepted options %+v", bad)
		}
	}
}

func TestReplayEnforcesBudgetAcrossReviewerAndJudge(t *testing.T) {
	t.Parallel()
	repo, set := replayFixture(t)
	failing := &fakeReviewer{t: t, repo: repo, answer: func(request reviewRequest) reviewResult {
		return reviewResult{Verdict: apiv1.Verdict{Decision: apiv1.VerdictPass, Synthesized: true}, CostUSD: min(1, request.BudgetRemainingUSD)}
	}}
	report, err := replay(context.Background(), set, gitReader{dir: repo.dir}, failing, matcher{},
		replayOptions{Split: splitEval, Variant: "v", MaxCases: 10, BudgetUSD: 1.5, Seed: "s", WorkDir: t.TempDir()})
	if err != nil || report.Failed != 2 || report.BudgetExhausted != 2 || report.Reviewed != 0 || report.TotalCostUSD != 1.5 || report.validate() != nil {
		t.Fatalf("synthesized verdict report = %+v, %v; want failed cases unscored but their spend capped by the budget", report, err)
	}

	judged := &fakeReviewer{t: t, repo: repo, answer: func(request reviewRequest) reviewResult {
		return reviewResult{CostUSD: min(0.5, request.BudgetRemainingUSD), Verdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Findings: []apiv1.Finding{
			{Class: apiv1.FindingMissingTests, Severity: apiv1.SeverityWarning, Location: "a.go", Message: "untested"},
			{Class: apiv1.FindingScopeCreep, Severity: apiv1.SeverityInfo, Location: "b.go", Message: "unrelated"},
		}}}
	}}
	report, err = replay(context.Background(), set, gitReader{dir: repo.dir}, judged, matcher{judge: &failingJudge{}},
		replayOptions{Split: splitEval, Variant: "v", MaxCases: 10, BudgetUSD: 1.4, Seed: "s", WorkDir: t.TempDir()})
	if err != nil || report.Failed != 1 || report.Reviewed != 1 || report.JudgeSkippedForBudget != 1 || report.BudgetExhausted != 2 || report.TotalCostUSD != 1.4 ||
		len(report.Results[0].JudgeDecisions) != 1 || report.Results[0].JudgeCostUSD != 0.2 || report.validate() != nil {
		t.Fatalf("judge failure report = %+v, %v; want the judge failure to fail its case with spend counted, and the next case's judging skipped once the budget ran out", report, err)
	}

	// A reviewer that spends its whole remaining budget leaves nothing for the
	// judge: the findings are recorded as skipped for budget, not judged.
	spendAll := &fakeReviewer{t: t, repo: repo, answer: func(request reviewRequest) reviewResult {
		return reviewResult{CostUSD: request.BudgetRemainingUSD, Verdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Findings: []apiv1.Finding{
			{Class: apiv1.FindingMissingTests, Severity: apiv1.SeverityWarning, Location: "a.go", Message: "untested"},
		}}}
	}}
	unjudged := &fakeJudge{}
	report, err = replay(context.Background(), set, gitReader{dir: repo.dir}, spendAll, matcher{judge: unjudged},
		replayOptions{Split: splitEval, Variant: "v", MaxCases: 10, BudgetUSD: 1, Seed: "s", WorkDir: t.TempDir()})
	if err != nil || unjudged.requests != 0 || report.Reviewed != 1 || report.BudgetExhausted != 3 || report.JudgeSkippedForBudget != 1 ||
		!report.Results[0].JudgeDecisions[0].SkippedForBudget || report.TotalCostUSD != 1 || report.validate() != nil {
		t.Fatalf("exhausted judge report = %+v, %v; want the finding skipped for budget and no judge call", report, err)
	}

	checkOverrunsStopReplay(t, repo, set)
}

// checkOverrunsStopReplay asserts a reviewer or judge that reports spending
// more than its remaining budget stops the replay, and that a work directory
// inside the live repository is refused.
func checkOverrunsStopReplay(t *testing.T, repo *fixtureRepo, set dataset) {
	t.Helper()
	// An adapter that reports spending more than it was given stops the run.
	overReviewer := &fakeReviewer{t: t, repo: repo, answer: func(request reviewRequest) reviewResult {
		return reviewResult{Verdict: apiv1.Verdict{Decision: apiv1.VerdictPass}, CostUSD: request.BudgetRemainingUSD + 1}
	}}
	overJudge := &fakeReviewer{t: t, repo: repo, answer: func(request reviewRequest) reviewResult {
		return reviewResult{CostUSD: 0.5, Verdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Findings: []apiv1.Finding{
			{Class: apiv1.FindingMissingTests, Severity: apiv1.SeverityWarning, Location: "a.go", Message: "untested"},
		}}}
	}}
	for name, run := range map[string]struct {
		review reviewer
		judge  judge
	}{"reviewer": {overReviewer, nil}, "judge": {overJudge, overspendingJudge{}}} {
		_, err := replay(context.Background(), set, gitReader{dir: repo.dir}, run.review, matcher{judge: run.judge},
			replayOptions{Split: splitEval, Variant: "v", MaxCases: 10, BudgetUSD: 1, Seed: "s", WorkDir: t.TempDir()})
		if !errors.Is(err, errBudgetOverrun) {
			t.Errorf("%s overspending its budget: replay error %v, want a budget overrun", name, err)
		}
	}
	if _, err := replay(context.Background(), set, gitReader{dir: repo.dir}, overReviewer, matcher{},
		replayOptions{Split: splitEval, Variant: "v", MaxCases: 1, BudgetUSD: 1, Seed: "s", WorkDir: filepath.Join(repo.dir, "work")}); err == nil {
		t.Error("replay accepted a work directory inside the live repository")
	}
}

func reportOf(variant string, caught map[string]bool, cost float64) evalReport {
	var results []caseResult
	for _, id := range []string{"case-a", "case-b"} {
		verdict := &apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Findings: []apiv1.Finding{{Class: apiv1.FindingSubstantive}}}
		result := caseResult{CaseID: id, Status: statusReviewed, CostUSD: cost, Verdict: verdict, Findings: []findingResult{{Class: "substantive"}}}
		if caught[id] {
			result.Caught, result.Findings[0].Match = true, matchLineOverlap
		}
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return sampleRank("s", results[i].CaseID) < sampleRank("s", results[j].CaseID) })
	set := dataset{Header: datasetHeader{Version: "v1", Digest: "sha256:d"}}
	return newReport(set, replayOptions{Split: splitEval, Variant: variant, Seed: "s", MaxCases: 2, BudgetUSD: 10}, 0, results)
}

func TestCompareGatesPromotionOnRegression(t *testing.T) {
	baseline := reportOf("baseline", map[string]bool{"case-a": true}, 1)
	better := reportOf("candidate", map[string]bool{"case-a": true, "case-b": true}, 1.5)
	comparison, err := compare(baseline, better)
	if err != nil || !comparison.Promotable || comparison.RecallDelta != 0.5 || comparison.CostPerBugDeltaUSD != 0.5 {
		t.Fatalf("better candidate comparison = %+v, %v", comparison, err)
	}
	worse := reportOf("candidate", map[string]bool{}, 1)
	comparison, err = compare(baseline, worse)
	if err != nil || comparison.Promotable || len(comparison.Regressions) != 3 {
		t.Fatalf("worse candidate comparison = %+v, %v; want recall, precision, and class regressions", comparison, err)
	}
	summary := worse.summary()
	summary.Comparison = &comparison
	if err := summary.Validate(); err != nil {
		t.Fatalf("comparison summary does not validate: %v", err)
	}

	otherDataset := reportOf("candidate", nil, 1)
	otherDataset.DatasetDigest = "sha256:other"
	fewerCases := reportOf("candidate", nil, 1)
	fewerCases.Results[1].Status = statusBudgetExhausted
	for name, candidate := range map[string]evalReport{
		"different dataset": otherDataset, "same variant": reportOf("baseline", nil, 1), "different cases": fewerCases,
	} {
		if _, err := compare(baseline, candidate); err == nil {
			t.Errorf("%s: compare accepted reports that are not comparable", name)
		}
	}
}

func writeReport(t *testing.T, report evalReport) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), report.Variant+".json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := encodeJSON(file, report); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIRequiresOptInAndPublishesComparison(t *testing.T) {
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"replay", "-dataset", "d.jsonl", "-reviewer-cmd", "reviewer", "-budget-usd", "1"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "-allow-real-reviewer") {
		t.Fatalf("replay without opt-in exit %d: %s", code, stderr.String())
	}
	if code := run(ctx, []string{"post"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown subcommand exit %d", code)
	}

	root := t.TempDir()
	baseline := writeReport(t, reportOf("baseline", map[string]bool{"case-a": true}, 1))
	worse := writeReport(t, reportOf("candidate", map[string]bool{}, 1))
	if code := run(ctx, []string{"compare", "-baseline", baseline, "-candidate", worse, "-publish", root}, &stdout, &stderr); code != 1 {
		t.Fatalf("regressing compare exit %d, want 1: %s", code, stderr.String())
	}
	published, err := readservice.ReadReviewerEval(root)
	if err != nil || published == nil || published.Comparison == nil || published.Comparison.Promotable || published.Variant != "candidate" {
		t.Fatalf("published comparison = %+v, %v", published, err)
	}

	training := reportOf("baseline", map[string]bool{"case-a": true}, 1)
	training.Split = splitTrain
	if code := run(ctx, []string{"publish", "-report", writeReport(t, training), "-instance", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("publishing a training-split report exit %d, want 1", code)
	}
}

func TestExtractTarSkipsGitDirAliases(t *testing.T) {
	for name, want := range map[string]bool{
		".git/hooks/post-commit": true, ".GIT/hooks/post-commit": true, ".git./config": true, "GIT~1/hooks/x": true,
		"sub/.git": true, "a.go": false, ".github/workflows/ci.yml": false, "pkg/.gitignore": false,
	} {
		if got := namesGitDir(name); got != want {
			t.Errorf("namesGitDir(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseLocation(t *testing.T) {
	for location, want := range map[string][3]any{
		"a.go":                {"a.go", 0, 0},
		"./pkg/a.go:12":       {"pkg/a.go", 12, 12},
		"pkg\\a.go:12-20":     {"pkg/a.go", 12, 20},
		"b/pkg/a.go:7:3":      {"pkg/a.go", 7, 7},
		"pkg/a.go#L4-L9 note": {"pkg/a.go", 4, 9},
	} {
		file, start, end := parseLocation(location)
		if got := [3]any{file, start, end}; got != want {
			t.Errorf("parseLocation(%q) = %v, want %v", location, got, want)
		}
	}
}

func TestLoadReportRejectsTamperedReports(t *testing.T) {
	t.Parallel()
	valid := func() evalReport { return reportOf("candidate", map[string]bool{"case-a": true}, 1) }
	if _, err := loadReport(writeReport(t, valid())); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	uncaught := func(report *evalReport) *caseResult {
		for index := range report.Results {
			if !report.Results[index].Caught {
				return &report.Results[index]
			}
		}
		t.Fatal("fixture report has no uncaught case")
		return nil
	}
	for name, tamper := range map[string]func(*evalReport){
		"overall recall":       func(report *evalReport) { report.Overall.Recall = 1 },
		"overall caught":       func(report *evalReport) { report.Overall.Caught = 2 },
		"class precision":      func(report *evalReport) { report.Classes[0].Precision = 1 },
		"reviewed count":       func(report *evalReport) { report.Reviewed = 1 },
		"total cost":           func(report *evalReport) { report.TotalCostUSD = 0.5 },
		"cost per bug":         func(report *evalReport) { report.CostPerBugUSD = 0.1 },
		"duplicate case":       func(report *evalReport) { report.Results[1].CaseID = report.Results[0].CaseID },
		"sampling order":       func(report *evalReport) { report.Results[0], report.Results[1] = report.Results[1], report.Results[0] },
		"sampled count":        func(report *evalReport) { report.Sampled = 3 },
		"over the case cap":    func(report *evalReport) { report.MaxCases = 1 },
		"over the budget":      func(report *evalReport) { report.BudgetUSD = 1 },
		"no seed":              func(report *evalReport) { report.Seed = "" },
		"caught with no match": func(report *evalReport) { uncaught(report).Caught = true },
		"judge match with no decision": func(report *evalReport) {
			result := uncaught(report)
			result.Caught, result.Findings[0].Match = true, matchJudge
		},
	} {
		report := valid()
		tamper(&report)
		if _, err := loadReport(writeReport(t, report)); err == nil {
			t.Errorf("%s: tampered report accepted", name)
		}
	}

	tampered := valid()
	tampered.Overall.Recall = 1
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	baseline := writeReport(t, reportOf("baseline", map[string]bool{"case-a": true}, 1))
	if code := run(context.Background(), []string{"compare", "-baseline", baseline, "-candidate", writeReport(t, tampered), "-publish", root}, &stdout, &stderr); code != 1 {
		t.Fatalf("compare with a tampered candidate exit %d, want 1", code)
	}
	if published, err := readservice.ReadReviewerEval(root); err != nil || published != nil {
		t.Fatalf("tampered comparison published %+v, %v", published, err)
	}
}

const (
	adapterHelperEnv = "REVIEWEREVAL_ADAPTER_HELPER"
	storedSecret     = "stored-forge-credential"
)

// adapterObservation is what the helper adapter saw of its environment.
type adapterObservation struct {
	Role    string            `json:"role"`
	Cwd     string            `json:"cwd"`
	Env     map[string]string `json:"env"`
	Secrets []string          `json:"secrets"`
}

// credentialPaths lists where gh and git persist forge credentials under the
// home and configuration directories getenv names.
func credentialPaths(getenv func(string) string) []string {
	var paths []string
	for _, location := range [][2]string{
		{"GH_CONFIG_DIR", "hosts.yml"}, {"HOME", filepath.Join(".config", "gh", "hosts.yml")},
		{"APPDATA", filepath.Join("GitHub CLI", "hosts.yml")}, {"HOME", ".git-credentials"},
		{"USERPROFILE", ".git-credentials"}, {"XDG_CONFIG_HOME", filepath.Join("git", "credentials")},
	} {
		if base := getenv(location[0]); base != "" {
			paths = append(paths, filepath.Join(base, location[1]))
		}
	}
	return paths
}

// TestAdapterHelperProcess is the adapter the isolation test runs as a
// subprocess: it records its working directory, environment, and any stored
// credential it can read, leaves a file in its working directory, and answers
// as a reviewer or judge. Run normally, it does nothing.
func TestAdapterHelperProcess(t *testing.T) {
	out := os.Getenv(adapterHelperEnv)
	if out == "" {
		return
	}
	var request map[string]any
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	_ = os.WriteFile(filepath.Join(cwd, "adapter-was-here"), nil, 0o644)
	observation := adapterObservation{Role: "reviewer", Cwd: cwd, Env: map[string]string{}}
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "GH_CONFIG_DIR", "GH_TOKEN",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
		if value, ok := os.LookupEnv(name); ok {
			observation.Env[name] = value
		}
	}
	for _, path := range credentialPaths(os.Getenv) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), storedSecret) {
			observation.Secrets = append(observation.Secrets, path)
		}
	}
	var answer any = reviewResult{Verdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Findings: []apiv1.Finding{
		{Class: apiv1.FindingMissingTests, Severity: apiv1.SeverityWarning, Location: "a.go", Message: "untested"},
	}}}
	if _, judging := request["finding"]; judging {
		observation.Role, answer = "judge", judgeDecision{Rationale: "observed"}
	}
	line, _ := json.Marshal(observation)
	file, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write(append(line, '\n'))
	_ = file.Close()
	_ = json.NewEncoder(os.Stdout).Encode(answer)
	os.Exit(0)
}

func TestAdaptersRunIsolatedFromCredentialsAndLiveTree(t *testing.T) {
	repo, set := replayFixture(t)
	set.Cases = set.Cases[:1]
	home := t.TempDir()
	operator := map[string]string{
		"HOME": home, "USERPROFILE": home, "APPDATA": filepath.Join(home, "AppData"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"), "GH_CONFIG_DIR": filepath.Join(home, ".config", "gh"),
	}
	for name, value := range operator {
		t.Setenv(name, value)
	}
	t.Setenv("GH_TOKEN", storedSecret)
	for _, path := range credentialPaths(func(name string) string { return operator[name] }) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(storedSecret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	observations := filepath.Join(t.TempDir(), "observations.jsonl")
	t.Setenv(adapterHelperEnv, observations)
	workDir, packageDir := t.TempDir(), t.TempDir()
	t.Chdir(packageDir)
	argv := []string{os.Args[0], "-test.run=^TestAdapterHelperProcess$"}
	before := repo.liveState(t)

	report, err := replay(context.Background(), set, gitReader{dir: repo.dir}, commandReviewer{argv: argv, workDir: workDir},
		matcher{judge: commandJudge{argv: argv, workDir: workDir}}, replayOptions{Split: splitEval, Variant: "v", MaxCases: 1, BudgetUSD: 1, Seed: "s", WorkDir: workDir})
	if err != nil || report.Reviewed != 1 || len(report.Results[0].JudgeDecisions) != 1 {
		t.Fatalf("replay through command adapters = %+v, %v; want one case reviewed and judged", report, err)
	}
	if after := repo.liveState(t); after != before {
		t.Fatal("an adapter changed the live repository")
	}
	if entries, err := os.ReadDir(packageDir); err != nil || len(entries) != 0 {
		t.Fatalf("an adapter wrote to the invoking working directory: %v %v", entries, err)
	}
	if entries, err := os.ReadDir(workDir); err != nil || len(entries) != 0 {
		t.Fatalf("adapter scratch directories were left behind: %v %v", entries, err)
	}
	data, err := os.ReadFile(observations)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var seen adapterObservation
		if err := json.Unmarshal([]byte(line), &seen); err != nil {
			t.Fatal(err)
		}
		roles[seen.Role] = true
		checkAdapterIsolated(t, seen, workDir, repo.dir)
	}
	if !roles["reviewer"] || !roles["judge"] {
		t.Fatalf("adapter roles observed = %v, want reviewer and judge", roles)
	}
}

// checkAdapterIsolated asserts an adapter ran in a scratch directory under
// workDir, outside the live repository, with isolated home directories, git
// credential helpers disabled, and no stored credential reachable.
func checkAdapterIsolated(t *testing.T, seen adapterObservation, workDir, liveDir string) {
	t.Helper()
	if len(seen.Secrets) > 0 || seen.Env["GH_TOKEN"] != "" {
		t.Errorf("%s adapter reached stored credentials %v (GH_TOKEN %q)", seen.Role, seen.Secrets, seen.Env["GH_TOKEN"])
	}
	if !inside(t, seen.Cwd, workDir) || inside(t, seen.Cwd, liveDir) {
		t.Errorf("%s adapter ran in %s, want a scratch directory under %s", seen.Role, seen.Cwd, workDir)
	}
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "GH_CONFIG_DIR"} {
		if !inside(t, seen.Env[name], workDir) {
			t.Errorf("%s adapter %s = %q, want an isolated directory under %s", seen.Role, name, seen.Env[name], workDir)
		}
	}
	if seen.Env["GIT_CONFIG_GLOBAL"] != os.DevNull || seen.Env["GIT_CONFIG_NOSYSTEM"] != "1" ||
		seen.Env["GIT_CONFIG_KEY_0"] != "credential.helper" || seen.Env["GIT_CONFIG_VALUE_0"] != "" {
		t.Errorf("%s adapter git config environment = %v, want global/system config off and credential helpers disabled", seen.Role, seen.Env)
	}
}

func inside(t *testing.T, path, dir string) bool {
	t.Helper()
	if path == "" {
		return false
	}
	resolvedFile, errFile := resolvedPath(path)
	resolvedDir, errDir := resolvedPath(dir)
	if err := errors.Join(errFile, errDir); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(resolvedDir, resolvedFile)
	return err == nil && filepath.IsLocal(relative)
}
