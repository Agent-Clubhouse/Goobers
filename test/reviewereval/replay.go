package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	// hardMaxCases caps any one replay run regardless of flags.
	hardMaxCases = 200
	// replayIdentity is the fixed author and date of reconstructed commits,
	// so a reconstruction is reproducible and carries no later metadata.
	replayIdentity = "reviewer-eval"
	replayDate     = "2000-01-01T00:00:00Z"
)

// reviewRequest is one reconstructed offending change for a reviewer: an
// isolated repository holding exactly the change's parent (BaseSHA) and the
// change itself (HeadSHA), with no later history, no remote, and no forge
// credentials.
type reviewRequest struct {
	CaseID             string  `json:"caseId"`
	Variant            string  `json:"variant"`
	Dir                string  `json:"dir"`
	BaseSHA            string  `json:"baseSha"`
	HeadSHA            string  `json:"headSha"`
	Title              string  `json:"title"`
	Diff               string  `json:"diff"`
	BudgetRemainingUSD float64 `json:"budgetRemainingUsd"`
}

// reviewResult is the reviewer's verdict on one reconstructed change and
// what producing it cost.
type reviewResult struct {
	Verdict apiv1.Verdict `json:"verdict"`
	CostUSD float64       `json:"costUsd"`
}

// reviewer runs merge-review on a reconstructed change. Tests use a fake;
// commandReviewer is the explicitly opted-in real invocation.
type reviewer interface {
	Review(ctx context.Context, request reviewRequest) (reviewResult, error)
}

// replayOptions are the replay guardrails: one split only, a bounded case
// count, a budget cap, and seeded deterministic sampling.
type replayOptions struct {
	Split     string
	Variant   string
	MaxCases  int
	BudgetUSD float64
	Seed      string
	WorkDir   string
}

func (options replayOptions) validate() error {
	if options.Split != splitEval && options.Split != splitTrain {
		return fmt.Errorf("replay split must be %q or %q", splitEval, splitTrain)
	}
	if strings.TrimSpace(options.Variant) == "" {
		return errors.New("replay needs a reviewer variant name")
	}
	if options.MaxCases < 1 || options.MaxCases > hardMaxCases {
		return fmt.Errorf("replay max cases must be within [1,%d]", hardMaxCases)
	}
	if !(options.BudgetUSD > 0) || math.IsInf(options.BudgetUSD, 0) {
		return errors.New("replay needs a positive budget cap")
	}
	if strings.TrimSpace(options.Seed) == "" {
		return errors.New("replay needs a sampling seed")
	}
	return nil
}

// sampleCases deterministically picks at most MaxCases cases of the
// requested split, ordered by a seeded hash of their IDs. Out-of-diff cases
// are excluded and counted, never replayed or scored as misses.
func sampleCases(set dataset, options replayOptions) (sampled []datasetCase, outOfDiff int) {
	for _, item := range set.Cases {
		if item.Split != options.Split {
			continue
		}
		if item.OutOfDiff {
			outOfDiff++
			continue
		}
		sampled = append(sampled, item)
	}
	rank := func(item datasetCase) string {
		sum := sha256.Sum256([]byte(options.Seed + "\x00" + item.ID))
		return hex.EncodeToString(sum[:])
	}
	sort.Slice(sampled, func(i, j int) bool { return rank(sampled[i]) < rank(sampled[j]) })
	if len(sampled) > options.MaxCases {
		sampled = sampled[:options.MaxCases]
	}
	return sampled, outOfDiff
}

// replay reconstructs and reviews each sampled case, scoring findings with
// the matcher. It stops reviewing once the budget is spent; cases left
// unreviewed are reported as not run, never as misses.
func replay(ctx context.Context, set dataset, live gitReader, review reviewer, match matcher, options replayOptions) (evalReport, error) {
	if err := options.validate(); err != nil {
		return evalReport{}, err
	}
	sampled, outOfDiff := sampleCases(set, options)
	results := make([]caseResult, 0, len(sampled))
	spent := 0.0
	for _, item := range sampled {
		result := caseResult{CaseID: item.ID, FixCommit: item.FixCommit, IntroducingCommit: item.IntroducingCommit}
		if spent >= options.BudgetUSD {
			result.Status = statusBudgetExhausted
			results = append(results, result)
			continue
		}
		reviewed, err := replayCase(ctx, live, review, item, options, options.BudgetUSD-spent)
		if err != nil {
			result.Status, result.Error = statusFailed, err.Error()
			results = append(results, result)
			continue
		}
		spent += reviewed.CostUSD
		scored, err := match.score(ctx, item, reviewed)
		if err != nil {
			return evalReport{}, fmt.Errorf("score case %s: %w", item.ID, err)
		}
		results = append(results, scored)
	}
	return newReport(set, options, outOfDiff, results), nil
}

func replayCase(ctx context.Context, live gitReader, review reviewer, item datasetCase, options replayOptions, budget float64) (reviewResult, error) {
	dir, err := os.MkdirTemp(options.WorkDir, "reviewer-eval-")
	if err != nil {
		return reviewResult{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	request, err := reconstruct(ctx, live, item.IntroducingCommit, dir)
	if err != nil {
		return reviewResult{}, fmt.Errorf("reconstruct %s: %w", item.IntroducingCommit, err)
	}
	request.CaseID, request.Variant, request.BudgetRemainingUSD = item.ID, options.Variant, budget
	result, err := review.Review(ctx, request)
	if err != nil {
		return reviewResult{}, fmt.Errorf("review: %w", err)
	}
	if result.Verdict.Synthesized || result.Verdict.Decision == "" {
		return reviewResult{}, errors.New("reviewer returned no reviewed verdict")
	}
	if !validCost(result.CostUSD) {
		return reviewResult{}, fmt.Errorf("reviewer reported invalid cost %v", result.CostUSD)
	}
	return result, nil
}

func validCost(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// reconstruct materializes an offending commit as a PR-shaped change in a
// fresh repository at dir: one commit holding its parent's tree, then one
// holding its own tree with its own message. Trees are exported with
// `git archive`, so the live repository is only read and the new repository
// shares no objects with it: later history, including the fix, is
// unreachable.
func reconstruct(ctx context.Context, live gitReader, commit, dir string) (reviewRequest, error) {
	parent, err := live.run(ctx, "rev-parse", "--verify", commit+"^")
	if err != nil {
		return reviewRequest{}, err
	}
	message, err := live.run(ctx, "log", "-1", "--format=%B", commit)
	if err != nil {
		return reviewRequest{}, err
	}
	scratch := func(args ...string) (string, error) {
		out, err := runGit(ctx, dir, []string{
			"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=" + replayIdentity, "GIT_AUTHOR_EMAIL=" + replayIdentity + "@invalid",
			"GIT_COMMITTER_NAME=" + replayIdentity, "GIT_COMMITTER_EMAIL=" + replayIdentity + "@invalid",
			"GIT_AUTHOR_DATE=" + replayDate, "GIT_COMMITTER_DATE=" + replayDate,
		}, args...)
		return strings.TrimSpace(string(out)), err
	}
	if _, err := scratch("init", "-q"); err != nil {
		return reviewRequest{}, err
	}
	var shas []string
	for _, step := range []struct{ rev, message string }{
		{strings.TrimSpace(parent), "base"}, {commit, message},
	} {
		if err := exportTree(ctx, live, step.rev, dir); err != nil {
			return reviewRequest{}, err
		}
		if _, err := scratch("add", "-A", "-f"); err != nil {
			return reviewRequest{}, err
		}
		if _, err := scratch("commit", "-q", "--allow-empty", "--no-verify", "-m", step.message); err != nil {
			return reviewRequest{}, err
		}
		sha, err := scratch("rev-parse", "HEAD")
		if err != nil {
			return reviewRequest{}, err
		}
		shas = append(shas, sha)
	}
	diff, err := scratch("diff", "--no-color", "--no-ext-diff", shas[0], shas[1])
	if err != nil {
		return reviewRequest{}, err
	}
	title, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	return reviewRequest{Dir: dir, BaseSHA: shas[0], HeadSHA: shas[1], Title: title, Diff: diff}, nil
}

// exportTree replaces dir's working tree (everything but .git) with rev's
// tree from the live repository.
func exportTree(ctx context.Context, live gitReader, rev, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != ".git" {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	archive, err := live.raw(ctx, "archive", "--format=tar", rev)
	if err != nil {
		return err
	}
	return extractTar(bytes.NewReader(archive), dir)
}

func extractTar(source io.Reader, dir string) error {
	reader := tar.NewReader(source)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(header.Name)
		if !filepath.IsLocal(name) || strings.HasPrefix(filepath.ToSlash(name), ".git/") || name == ".git" {
			continue
		}
		target := filepath.Join(dir, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeTarFile(reader, target, header.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Symlinks are kept as files holding their target, so a link can
			// never point the review outside the isolated tree.
			if err := os.WriteFile(target, []byte(header.Linkname), 0o644); err != nil {
				return err
			}
		}
	}
}

func writeTarFile(reader io.Reader, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, reader); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// commandReviewer is the opt-in real reviewer: an operator-supplied adapter
// command that runs the production merge-review reviewer on the isolated
// repository. It runs in that repository with forge credentials stripped,
// receives the request as JSON on stdin, and prints a reviewResult as JSON.
type commandReviewer struct {
	argv []string
}

func (reviewer commandReviewer) Review(ctx context.Context, request reviewRequest) (reviewResult, error) {
	var result reviewResult
	if err := runAdapter(ctx, reviewer.argv, request.Dir, request, &result); err != nil {
		return reviewResult{}, err
	}
	return result, nil
}

// runAdapter runs an adapter command with input as JSON on stdin and decodes
// its stdout into output. The environment carries no forge credentials.
func runAdapter(ctx context.Context, argv []string, dir string, input, output any) error {
	if len(argv) == 0 {
		return errors.New("adapter command is empty")
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(stdout.Bytes(), output); err != nil {
		return fmt.Errorf("decode %s output: %w", argv[0], err)
	}
	return nil
}
