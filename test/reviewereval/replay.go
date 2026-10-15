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
	sort.Slice(sampled, func(i, j int) bool {
		return sampleRank(options.Seed, sampled[i].ID) < sampleRank(options.Seed, sampled[j].ID)
	})
	if len(sampled) > options.MaxCases {
		sampled = sampled[:options.MaxCases]
	}
	return sampled, outOfDiff
}

// errBudgetOverrun marks an adapter that reported spending more than the
// remaining budget it was given. Replay stops at once rather than record a
// run whose spend broke its cap.
var errBudgetOverrun = errors.New("adapter overran its remaining budget")

// costEpsilon absorbs float rounding when comparing spend with a budget.
const costEpsilon = 1e-9

// sampleRank orders cases for sampling: a seeded hash of the case ID.
func sampleRank(seed, id string) string {
	sum := sha256.Sum256([]byte(seed + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

// replay reconstructs and reviews each sampled case, scoring findings with
// the matcher. Every reviewer and judge call is told the budget it has left
// and may not spend more; once the budget is spent the remaining cases are
// reported as not run, never as misses.
func replay(ctx context.Context, set dataset, live gitReader, review reviewer, match matcher, options replayOptions) (evalReport, error) {
	if err := options.validate(); err != nil {
		return evalReport{}, err
	}
	if err := checkWorkDir(options.WorkDir, live.dir); err != nil {
		return evalReport{}, err
	}
	sampled, outOfDiff := sampleCases(set, options)
	results := make([]caseResult, 0, len(sampled))
	spent := 0.0
	for _, item := range sampled {
		result := caseResult{CaseID: item.ID, FixCommit: item.FixCommit, IntroducingCommit: item.IntroducingCommit}
		remaining := options.BudgetUSD - spent
		if remaining <= costEpsilon {
			result.Status = statusBudgetExhausted
			results = append(results, result)
			continue
		}
		reviewed, err := replayCase(ctx, live, review, item, options, remaining)
		spent += reviewed.CostUSD
		if errors.Is(err, errBudgetOverrun) {
			return evalReport{}, fmt.Errorf("case %s: %w (spent $%.4f of $%.4f)", item.ID, err, spent, options.BudgetUSD)
		}
		if err != nil {
			result.Status, result.Error, result.CostUSD = statusFailed, err.Error(), round4(reviewed.CostUSD)
			results = append(results, result)
			continue
		}
		scored, err := match.score(ctx, item, reviewed, options.BudgetUSD-spent)
		spent += scored.JudgeCostUSD
		if errors.Is(err, errBudgetOverrun) {
			return evalReport{}, fmt.Errorf("case %s: %w (spent $%.4f of $%.4f)", item.ID, err, spent, options.BudgetUSD)
		}
		if err != nil {
			result.Status, result.Error = statusFailed, fmt.Sprintf("score: %v", err)
			result.CostUSD, result.JudgeCostUSD = scored.CostUSD, scored.JudgeCostUSD
			result.JudgeDecisions = scored.JudgeDecisions
			results = append(results, result)
			continue
		}
		results = append(results, scored)
	}
	return newReport(set, options, outOfDiff, results), nil
}

// checkWorkDir refuses a replay work directory inside the live repository,
// so neither a reconstruction nor an adapter ever runs in the live tree.
func checkWorkDir(workDir, liveDir string) error {
	if workDir == "" {
		workDir = os.TempDir()
	}
	work, errWork := resolvedPath(workDir)
	live, errLive := resolvedPath(liveDir)
	if err := errors.Join(errWork, errLive); err != nil {
		return err
	}
	if relative, err := filepath.Rel(live, work); err == nil && filepath.IsLocal(relative) {
		return fmt.Errorf("replay work directory %s is inside the live repository %s", workDir, liveDir)
	}
	return nil
}

// resolvedPath resolves symlinks in the longest existing prefix of path, so a
// directory that does not exist yet still compares with resolved paths.
func resolvedPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for current := absolute; ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if filepath.Dir(current) == current {
			return absolute, nil
		}
		rest = filepath.Join(filepath.Base(current), rest)
	}
}

// replayCase reconstructs and reviews one case. A review the harness rejects
// still returns the cost the reviewer reported, so the budget counts it.
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
	if !validCost(result.CostUSD) {
		return reviewResult{}, fmt.Errorf("reviewer reported invalid cost %v", result.CostUSD)
	}
	if result.CostUSD > budget+costEpsilon {
		return reviewResult{CostUSD: result.CostUSD}, fmt.Errorf("reviewer reported $%.4f with $%.4f left: %w", result.CostUSD, budget, errBudgetOverrun)
	}
	if result.Verdict.Synthesized || result.Verdict.Decision == "" {
		return reviewResult{CostUSD: result.CostUSD}, errors.New("reviewer returned no reviewed verdict")
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
		if !filepath.IsLocal(name) || namesGitDir(header.Name) {
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

// namesGitDir reports whether any component of an archive path could resolve
// to a .git directory, including the aliases case-insensitive or Windows
// filesystems accept (".GIT", ".git.", "GIT~1"), so an exported tree can never
// write into the scratch repository's own .git and plant a hook.
func namesGitDir(name string) bool {
	for _, component := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		component = strings.TrimRight(component, ". ")
		if strings.EqualFold(component, ".git") || strings.EqualFold(component, "git~1") {
			return true
		}
	}
	return false
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
// repository. It runs in that repository with an isolated home and no forge
// credentials, receives the request as JSON on stdin, and prints a
// reviewResult as JSON.
type commandReviewer struct {
	argv    []string
	workDir string
}

func (reviewer commandReviewer) Review(ctx context.Context, request reviewRequest) (reviewResult, error) {
	var result reviewResult
	if err := runAdapter(ctx, reviewer.argv, reviewer.workDir, request.Dir, request, &result); err != nil {
		return reviewResult{}, err
	}
	return result, nil
}

// runAdapter runs an adapter command with input as JSON on stdin and decodes
// its stdout into output. It runs in dir, or in a fresh scratch directory
// when dir is empty, never in the live checkout, with adapterEnvironment.
func runAdapter(ctx context.Context, argv []string, workDir, dir string, input, output any) error {
	if len(argv) == 0 {
		return errors.New("adapter command is empty")
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	sandbox, err := os.MkdirTemp(workDir, "reviewer-eval-adapter-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(sandbox) }()
	env, err := adapterEnvironment(sandbox)
	if err != nil {
		return err
	}
	if dir == "" {
		dir = filepath.Join(sandbox, "work")
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
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

// isolatedHomeVariables are the variables through which a process finds the
// operator's persisted configuration and credentials (gh hosts, git
// credential stores, global git config). Adapters get fresh empty ones.
var isolatedHomeVariables = map[string]string{
	"HOME": "home", "USERPROFILE": "home", "APPDATA": "appdata", "LOCALAPPDATA": "localappdata",
	"XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_STATE_HOME": "state", "XDG_CACHE_HOME": "cache",
	"GH_CONFIG_DIR": "gh",
}

// adapterEnvironment is gitEnvironment with every home and configuration
// directory pointed into sandbox, global and system git config ignored, and
// git credential helpers disabled, so an adapter can reach neither the
// operator's stored forge credentials nor their git configuration.
func adapterEnvironment(sandbox string) ([]string, error) {
	env := make([]string, 0, len(os.Environ())+len(isolatedHomeVariables)+8)
	for _, entry := range gitEnvironment() {
		name, _, _ := strings.Cut(entry, "=")
		if !isolatedVariable(name) {
			env = append(env, entry)
		}
	}
	names := make([]string, 0, len(isolatedHomeVariables))
	for name := range isolatedHomeVariables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dir := filepath.Join(sandbox, isolatedHomeVariables[name])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		env = append(env, name+"="+dir)
	}
	return append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
		"GCM_INTERACTIVE=never",
	), nil
}

func isolatedVariable(name string) bool {
	upper := strings.ToUpper(name)
	if _, ok := isolatedHomeVariables[upper]; ok {
		return true
	}
	switch upper {
	case "HOMEDRIVE", "HOMEPATH", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
		"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GCM_INTERACTIVE":
		return true
	}
	return strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_") ||
		strings.HasPrefix(upper, "GH_") || strings.HasPrefix(upper, "GITHUB_")
}
