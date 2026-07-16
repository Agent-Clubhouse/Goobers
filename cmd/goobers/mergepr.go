package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// runMergePR implements the `goobers merge-pr` built-in stage kind (issue
// #360): the provider-level conjunctive auto-merge action `merge-review`
// drives. It merges a PR only when EVERY independent conjunct holds —
// verdict=pass, CI green, not a draft, and the SHA-pin (headSha/baseSha)
// still matches the PR's LIVE state — never a bare self-approval, and never
// trusting a caller-supplied "still valid" claim instead of re-polling
// (docs/design/v0/pr-lifecycle-loop.md §7/D6).
//
// A PR missing any one conjunct is a normal, expected outcome (the PR just
// isn't ready yet), not a stage failure: it exits 0 with merged=false and a
// human-readable reason in the declared result file, so a downstream gate
// can branch on Outputs["merged"] — the same philosophy as ci-poll, whose
// stage always succeeds at determining an outcome even when that outcome is
// "still pending" (internal/executor/cipoll.go's ciPollOutcome doc). Only a
// genuine provider/config error (missing capability, unresolvable repo, a
// merge attempt that should have succeeded but didn't) is a business error.
func runMergePR(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("merge-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers merge-pr [path]\n\n"+
			"Merge a pull request, but only when every independent conjunct holds:\n"+
			"verdict=pass, CI green, not a draft, and the SHA-pin still matches the\n"+
			"PR's live head/base (never a bare self-approval). Declared inputs:\n"+
			"pullNumber, verdict, headSha, baseSha (all required), advisoryMode\n"+
			"(default false — report only, no merge attempted), commitMessage,\n"+
			"resultFile (default merge-result.json). Successful merges also report\n"+
			"headBranch and branchCleanup (deleted, skipped-stacked, or failed).\n"+
			"Exit codes: 0 = evaluated\n"+
			"(merged or not — see the result file's \"merged\" field), 1 = business\n"+
			"error (missing capability/config, malformed inputs, provider failure),\n"+
			"2 = usage/IO error.\n")
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
	// The capability check IS the "capability absent → refused" acceptance
	// criterion: providerToken fails closed (no merge attempted, no PR
	// state even polled) unless this stage's declaration actually grants
	// github:pr:merge — the same fail-closed mechanism every other
	// provider-chain subcommand already relies on.
	token, err := providerToken(capability.GitHubPRMerge)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))

	pullNumber := providerInput("pullNumber", "")
	if pullNumber == "" {
		pf(stderr, "error: pullNumber input is required\n")
		return 1
	}
	verdict := providerInput("verdict", "")
	if verdict == "" {
		pf(stderr, "error: verdict input is required\n")
		return 1
	}
	if !apiv1.VerdictDecision(verdict).IsValid() {
		pf(stderr, "error: verdict input %q is not a known verdict decision\n", verdict)
		return 1
	}
	expectedHeadSHA := providerInput("headSha", "")
	if expectedHeadSHA == "" {
		pf(stderr, "error: headSha input is required (the SHA-pin, D6)\n")
		return 1
	}
	expectedBaseSHA := providerInput("baseSha", "")
	if expectedBaseSHA == "" {
		pf(stderr, "error: baseSha input is required (the SHA-pin, D6)\n")
		return 1
	}
	advisoryMode := providerInput("advisoryMode", "false") == "true"
	commitMessage := providerInput("commitMessage", "")
	resultFile := providerInput("resultFile", "merge-result.json")

	ctx := context.Background()
	// Independent, live re-check (D6) — never trust a caller-supplied
	// "still valid" claim for CI/draft/SHA-pin; always re-poll the PR's
	// actual current state right before deciding.
	poll, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: repo, PullID: pullNumber})
	if err != nil {
		return failProviderStage(stderr, "poll pull request", err, "merge-result.json")
	}

	var reasons []string
	if apiv1.VerdictDecision(verdict) != apiv1.VerdictPass {
		reasons = append(reasons, fmt.Sprintf("verdict is %q, want pass", verdict))
	}
	if poll.CheckState != providers.CheckStatePassing {
		reasons = append(reasons, fmt.Sprintf("CI is %q, want passing", poll.CheckState))
	}
	if poll.Draft {
		reasons = append(reasons, "pull request is a draft")
	}
	if poll.HeadSHA != expectedHeadSHA {
		reasons = append(reasons, fmt.Sprintf("head moved: verdict pinned to %s, PR is now at %s — verdict is stale", expectedHeadSHA, poll.HeadSHA))
	}
	if poll.BaseSHA != expectedBaseSHA {
		reasons = append(reasons, fmt.Sprintf("base moved: verdict pinned to %s, PR is now based on %s — verdict is stale", expectedBaseSHA, poll.BaseSHA))
	}
	if advisoryMode {
		reasons = append(reasons, "advisory mode: no merge attempted")
	}

	if len(reasons) > 0 {
		if err := writeMergeResult(resultFile, pullNumber, false, "", reasons, nil); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		pf(stdout, "not merged (pr #%s): %s\n", pullNumber, strings.Join(reasons, "; "))
		return 0
	}

	result, err := provider.MergePullRequest(ctx, providers.MergePullRequestRequest{
		Repository: repo, PullID: pullNumber, ExpectedHeadSHA: expectedHeadSHA, CommitMessage: commitMessage,
	})
	if err != nil {
		return failProviderStage(stderr, "merge pull request", err, "merge-result.json")
	}
	var cleanup *mergeBranchCleanup
	if result.Merged {
		outcome := cleanupMergedBranch(ctx, repo, poll.HeadBranch, provider)
		cleanup = &outcome
		if outcome.Error != "" {
			pf(stderr, "warning: merged pr #%s but branch cleanup failed: %s\n", pullNumber, outcome.Error)
		} else {
			pf(stdout, "branch cleanup %s (%s)\n", outcome.Status, outcome.HeadBranch)
		}
	}
	if err := writeMergeResult(resultFile, pullNumber, result.Merged, result.MergeSHA, nil, cleanup); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merged pr #%s (%s)\n", pullNumber, result.MergeSHA)
	return 0
}

type mergeBranchCleanup struct {
	Status     string
	HeadBranch string
	Error      string
}

func cleanupMergedBranch(ctx context.Context, repo providers.RepositoryRef, headBranch string, prProvider *providers.GitHubProvider) mergeBranchCleanup {
	out := mergeBranchCleanup{HeadBranch: headBranch}
	recorder := sidecarMutationRecorder{kind: "branch"}
	fail := func(err error) mergeBranchCleanup {
		out.Status = "failed"
		out.Error = err.Error()
		return out
	}
	if headBranch == "" {
		return fail(fmt.Errorf("merged pull request did not report a head branch"))
	}

	stacked, err := prProvider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository:     repo,
		Base:           headBranch,
		SkipCheckState: true,
	})
	if err != nil {
		return fail(fmt.Errorf("check stacked pull requests for %q: %w", headBranch, err))
	}
	if len(stacked) > 0 {
		out.Status = "skipped-stacked"
		return out
	}

	token, err := providerToken(capability.GitHubBranchDelete)
	if err != nil {
		return fail(err)
	}
	branchProvider := newGitHubProvider(token, providers.WithMutationRecorder(recorder))
	if err := branchProvider.DeleteBranch(ctx, providers.DeleteBranchRequest{Repository: repo, Name: headBranch}); err != nil {
		return fail(fmt.Errorf("delete branch %q: %w", headBranch, err))
	}
	out.Status = "deleted"
	return out
}

// writeMergeResult writes the declared result file's flat JSON — selectedNumber
// (string, always present), merged (bool, always present), mergeSha (on success),
// reason (a semicolon-joined list of unmet conjuncts, on refusal), and
// headBranch/branchCleanup/branchCleanupError (after a merge) — matching
// InputResultFile's flat-scalar-merge convention (internal/executor/shell.go's
// mergeResultFileOutputs). selectedNumber is echoed so the task after merge-gate
// can receive it through InputsFrom; merged lets that gate branch with zero new
// plumbing.
func writeMergeResult(path, selectedNumber string, merged bool, mergeSHA string, reasons []string, cleanup *mergeBranchCleanup) error {
	out := map[string]interface{}{"selectedNumber": selectedNumber, "merged": merged}
	if mergeSHA != "" {
		out["mergeSha"] = mergeSHA
	}
	if len(reasons) > 0 {
		out["reason"] = strings.Join(reasons, "; ")
	}
	if cleanup != nil {
		out["branchCleanup"] = cleanup.Status
		out["headBranch"] = cleanup.HeadBranch
		if cleanup.Error != "" {
			out["branchCleanupError"] = cleanup.Error
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal merge result: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
