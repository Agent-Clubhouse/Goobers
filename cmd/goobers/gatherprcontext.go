package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// prThreadComment is one comment on the PR thread — human/other-agent review
// feedback, or a prior merge-review verdict comment — surfaced as context
// for whatever addresses the PR next (design doc §5: pr-remediation reads
// "the Verdict artifact, PR-thread comments, and behind/conflict state").
type prThreadComment struct {
	Author    string `json:"author,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// runGatherPRContext implements `goobers gather-pr-context` (issue #362):
// pr-remediation's entrypoint, replacing implementation's query-backlog head
// (design doc §5 — "the one genuinely new executor entrypoint"). Selects one
// open, goober-authored PR labeled needs-remediation or reporting failing CI,
// checks out ITS branch into this stage's worktree (replacing whatever branch
// the runner's worktree provisioning defaulted to — pr-remediation re-enters
// on an EXISTING PR, it does not open a new one), and loads the merge-review
// Verdict + PR-thread comments + whether the base has advanced since this PR
// branched, as context for the stages that follow (#363's rebase +
// finding-driven routing).
func runGatherPRContext(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gather-pr-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers gather-pr-context [path]\n\n"+
			"Select one open, goober-authored PR labeled goobers:needs-remediation\n"+
			"or reporting failing CI, check out its branch into this stage's\n"+
			"worktree, and load the latest merge-review verdict + PR-thread comments\n"+
			"+ whether the base has advanced since this PR branched, writing them to\n"+
			"the declared result file. [path] is the instance root (matching\n"+
			"pr-select/apply-verdict), defaulting to GOOBERS_INSTANCE_ROOT; git\n"+
			"operations run against the stage's actual worktree (the process's\n"+
			"current directory), not path — same split push-branch already relies\n"+
			"on. Exit codes: 0 = context gathered (or no-work if no PR is eligible),\n"+
			"1 = business error, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// issues:write and repo:push are both used below (ListComments hits the
	// issues API; the checkout is a git operation) — both checked explicitly
	// before any call is made, matching #360/#361's capability-absent-refuses-
	// first contract. In V0 all three resolve to the identical repo credential
	// (runnerwiring.go's credentialedCapabilities), so only prToken is
	// actually needed to construct the provider.
	if _, err := providerToken(capability.GitHubIssuesWrite); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(prToken)

	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", "goobers/")

	ctx := context.Background()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	})
	if err != nil {
		pf(stderr, "error: list pull requests: %v\n", err)
		return 1
	}

	var eligible []providers.PullRequestSummary
	for _, pr := range prs {
		if hasAnyLabel(pr.Labels, []string{needsRemediationLabel}) ||
			pr.CheckState == providers.CheckStateFailing {
			eligible = append(eligible, pr)
		}
	}
	if len(eligible) == 0 {
		return writeNoWorkResult(stdout, stderr, "no PR needs remediation this cycle")
	}

	// Deterministic ordering (ascending PR number, i.e. oldest-flagged-first)
	// — mirrors pr-select: a stable, reproducible choice among however many
	// are eligible, not priority/FIFO ordering (#350's job).
	selected := eligible[0]
	for _, pr := range eligible[1:] {
		if pr.Number < selected.Number {
			selected = pr
		}
	}

	if _, err := checkoutExistingBranch(".", selected.Head, pushToken); err != nil {
		pf(stderr, "error: checkout PR #%d's branch %q: %v\n", selected.Number, selected.Head, err)
		return 1
	}

	behind, err := isBehindBase(".", selected.BaseSHA)
	if err != nil {
		pf(stderr, "error: check base ancestry for PR #%d: %v\n", selected.Number, err)
		return 1
	}

	rawComments, err := provider.ListComments(ctx, repo, strconv.Itoa(selected.Number))
	if err != nil {
		pf(stderr, "error: list comments on PR #%d: %v\n", selected.Number, err)
		return 1
	}
	// Latest comment carrying an embedded payload wins (a PR can accumulate
	// several merge-review cycles' worth of comments; only the most recent
	// verdict is still actionable).
	var verdict *apiv1.Verdict
	for i := len(rawComments) - 1; i >= 0; i-- {
		if v, ok := parseVerdictComment(rawComments[i].Body); ok {
			verdict = &v
			break
		}
	}
	comments := make([]prThreadComment, 0, len(rawComments))
	for _, c := range rawComments {
		createdAt := ""
		if c.CreatedAt != nil {
			createdAt = c.CreatedAt.Format(time.RFC3339)
		}
		comments = append(comments, prThreadComment{Author: c.Author, Body: c.Body, CreatedAt: createdAt})
	}

	// hasSubstantiveFindings is a plain "true"/"false" STRING, not a native
	// bool: internal/executor's InputResultFile convention only threads
	// string-valued top-level result-file keys through Task.InputsFrom into
	// a downstream stage's actual GOOBERS_INPUT_* env var (a bool/object
	// value survives into the run's Outputs map fine, but is silently
	// dropped at that later step) — #363's rebase-pr is the first consumer
	// and needs this to arrive intact. selectedNumber is stringified for the
	// exact same reason (matching pr-select's own strconv.Itoa convention).
	hasSubstantiveFindings := "false"
	if verdict != nil {
		for _, f := range verdict.Findings {
			if f.Class == apiv1.FindingSubstantive {
				hasSubstantiveFindings = "true"
				break
			}
		}
	}

	resultFile := providerInput("resultFile", "pr-context.json")
	data, err := json.MarshalIndent(map[string]interface{}{
		"selectedNumber":         strconv.Itoa(selected.Number),
		"head":                   selected.Head,
		"base":                   selected.Base,
		"headSha":                selected.HeadSHA,
		"baseSha":                selected.BaseSHA,
		"isBehindBase":           behind,
		"hasSubstantiveFindings": hasSubstantiveFindings,
		"verdict":                verdict,
		"comments":               comments,
	}, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal pr context: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "gathered context for PR #%d (%s): behind=%v, %d comment(s)\n", selected.Number, selected.Head, behind, len(comments))
	return 0
}

// checkoutExistingBranch fetches branch from origin and checks it out at
// dir, replacing whatever the runner's worktree provisioning checked out by
// default (a fresh run-scoped branch off base — irrelevant here, since
// pr-remediation re-enters on an EXISTING PR's branch rather than opening a
// new one). EVERY stage in pr-remediation.yaml gets its own fresh worktree
// (see internal/runner's per-stage-attempt worktree provisioning), so this
// is not a one-time setup step — rebase-pr (#363) calls it again for
// exactly this reason, not out of redundancy. Authenticated via gitAuthEnv,
// shared with push-branch's gitPushBranch (#237): never a URL-embedded
// credential, never persisted to disk.
//
// Returns the branch's remote SHA at the moment of THIS fetch — rebase-pr's
// eventual force-with-lease push must compare against this exact value (the
// state this stage started from), never a value re-resolved right before
// pushing: re-resolving immediately before the push would make the lease
// tautological (it would always match whatever just landed), silently
// defeating the "don't clobber a concurrent push" guarantee force-with-lease
// exists for.
func checkoutExistingBranch(dir, branch, token string) (fetchedSHA string, err error) {
	url, err := originURL(dir)
	if err != nil {
		return "", err
	}
	env := gitAuthEnv(token)
	fetch := exec.Command("git", "fetch", url, "refs/heads/"+branch)
	fetch.Dir = dir
	fetch.Env = env
	if out, err := fetch.CombinedOutput(); err != nil {
		return "", fmt.Errorf("fetch %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	rev := exec.Command("git", "rev-parse", "FETCH_HEAD")
	rev.Dir = dir
	out, err := rev.Output()
	if err != nil {
		return "", fmt.Errorf("resolve fetched SHA for %s: %w", branch, err)
	}
	fetchedSHA = strings.TrimSpace(string(out))
	checkout := exec.Command("git", "checkout", "-B", branch, "FETCH_HEAD")
	checkout.Dir = dir
	if out, err := checkout.CombinedOutput(); err != nil {
		return "", fmt.Errorf("checkout %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	return fetchedSHA, nil
}

// isBehindBase reports whether baseSHA is NOT an ancestor of the checked-out
// HEAD at dir — i.e. the base branch has advanced since this PR branched, so
// a rebase (issue #363) will be needed. This only detects staleness; it
// never attempts the rebase itself (design doc §5 D3: routing is
// finding-driven, never rebase-driven — that decision belongs to the stage
// after this one).
func isBehindBase(dir, baseSHA string) (bool, error) {
	if baseSHA == "" {
		return false, fmt.Errorf("PR has no recorded base SHA")
	}
	cmd := exec.Command("git", "merge-base", "--is-ancestor", baseSHA, "HEAD")
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s HEAD: %w", baseSHA, err)
}
