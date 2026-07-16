package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// siblingPR is one OTHER open PR's evidence for the holistic review — what
// it touches and its own state, so the reviewer can spot cross-PR
// conflict/drift the in-run reviewer (which sees only one diff) never can
// (issue #359, design doc §3).
type siblingPR struct {
	Number     int      `json:"number"`
	URL        string   `json:"url"`
	Head       string   `json:"head"`
	Draft      bool     `json:"draft"`
	Labels     []string `json:"labels,omitempty"`
	CheckState string   `json:"checkState"`
	Files      []string `json:"files"`
}

// runGatherSiblingContext implements `goobers gather-sibling-context`
// (issue #359): loads every OTHER open PR's touched files + state as
// evidence context for the holistic review gate that follows — the
// sibling-set context stage the design doc calls "where the cross-PR value
// lives; without it the review degrades back to single-diff and catches
// nothing cross-cutting." Deliberately queries ALL other open PRs (not just
// ones pr-select would itself find eligible) — a sibling that's draft, red,
// or already labeled is still relevant evidence (e.g. "PR #12 touches the
// same file but isn't ready yet").
//
// Per-sibling evidence is memoized across runs (issue #523,
// siblingcache.go): the open-PR list itself is always queried fresh — it is
// the freshness probe, one request regardless of PR count, and the source
// of every volatile field (draft/labels/head SHA) — but a sibling whose
// head SHA is unchanged since the last gather reuses its cached files and
// terminal check state instead of costing three more requests per run.
func runGatherSiblingContext(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gather-sibling-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers gather-sibling-context [--no-cache] [path]\n\n"+
			"Load the other open goober-authored PRs' touched files + state as\n"+
			"evidence for the holistic review (a workflow stage, follows\n"+
			"pr-select). Requires selectedNumber/selectedHead/selectedBase inputs\n"+
			"(Task.InputsFrom pr-select's own outputs). Sibling files/check state\n"+
			"are memoized per head SHA under the instance scheduler dir; --no-cache\n"+
			"bypasses the memo entirely (neither read nor written) to force a\n"+
			"fully fresh gather. Exit codes: 0 = context gathered (possibly empty\n"+
			"— no siblings is not an error), 1 = business error, 2 = usage/IO\n"+
			"error.\n")
	}
	noCache := fs.Bool("no-cache", false, "bypass the sibling-context cache (debug/remediation escape hatch)")
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
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token)

	selectedNumberStr := providerInput("selectedNumber", "")
	if selectedNumberStr == "" {
		pf(stderr, "error: selectedNumber is required (inputsFrom pr-select's number output)\n")
		return 1
	}
	selectedNumber, err := strconv.Atoi(selectedNumberStr)
	if err != nil {
		pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
		return 1
	}
	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", "goobers/")

	ctx := context.Background()
	// SkipCheckState: the list is the always-fresh probe (one request), but
	// per-candidate check-state resolution is two more requests per PR —
	// resolved below only for siblings whose cached state isn't reusable.
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix, SkipCheckState: true,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, "sibling-context.json")
	}

	schedulerDir := layoutFor(root).SchedulerDir()
	var cached map[string]siblingCacheEntry
	if !*noCache {
		cached = loadSiblingCache(schedulerDir, stderr)
	}
	next := make(map[string]siblingCacheEntry, len(prs))

	var selectedHeadSHA, selectedBaseSHA string
	reused := 0
	siblings := make([]siblingPR, 0, len(prs))
	for _, pr := range prs {
		if pr.Number == selectedNumber {
			// Capture the selected PR's OWN current SHAs from this same
			// fresh query — this is what the review gate's Verdict should
			// pin against (design doc §6 D6), not whatever pr-select saw
			// several stages ago.
			selectedHeadSHA, selectedBaseSHA = pr.HeadSHA, pr.BaseSHA
			// Keep its still-valid memo through the save's prune-to-open-set:
			// this PR is a *sibling* from every other run's perspective, and
			// merge-review cycles through selections — evicting here would
			// force the very next run to re-fetch it.
			if prior, ok := cached[strconv.Itoa(pr.Number)]; ok && prior.HeadSHA == pr.HeadSHA {
				next[strconv.Itoa(pr.Number)] = prior
			}
			continue
		}
		key := strconv.Itoa(pr.Number)
		prior, hit := cached[key]
		hit = hit && prior.HeadSHA == pr.HeadSHA
		paths := prior.Files
		if !hit {
			files, ferr := provider.PullRequestFiles(ctx, repo, key)
			if ferr != nil {
				return failProviderStage(stderr, fmt.Sprintf("list files for PR #%d", pr.Number), ferr, "sibling-context.json")
			}
			paths = make([]string, 0, len(files))
			for _, f := range files {
				paths = append(paths, f.Path)
			}
		} else {
			reused++
		}
		checkState := prior.CheckState
		if !hit || !checkStateTerminal(checkState) {
			checkState, err = provider.RefCheckState(ctx, repo, pr.HeadSHA)
			if err != nil {
				return failProviderStage(stderr, fmt.Sprintf("check state for PR #%d", pr.Number), err, "sibling-context.json")
			}
		}
		next[key] = siblingCacheEntry{HeadSHA: pr.HeadSHA, CheckState: checkState, Files: paths}
		siblings = append(siblings, siblingPR{
			Number: pr.Number, URL: pr.URL, Head: pr.Head, Draft: pr.Draft,
			Labels: pr.Labels, CheckState: string(checkState), Files: paths,
		})
	}

	// Persist before the selected-vanished check: sibling evidence gathered
	// on a run that ends up moot is still valid memo for the next run.
	if !*noCache {
		if err := saveSiblingCache(schedulerDir, next); err != nil {
			pf(stderr, "warning: persist sibling-context cache: %v\n", err)
		}
	}

	if selectedHeadSHA == "" {
		// The selected PR vanished from the eligible list between pr-select
		// and here (merged/closed/retargeted mid-cycle) — nothing to review.
		return writeNoWorkResult(stdout, stderr, "selected PR is no longer open")
	}

	resultFile := providerInput("resultFile", "sibling-context.json")
	data, err := json.MarshalIndent(map[string]interface{}{
		// selectedNumber is emitted as a STRING (selectedNumberStr, not the
		// parsed int), matching pr-select's "number":"403" and apply-verdict's
		// strconv.Atoi consumer — one type end-to-end (#413). This is
		// load-bearing, not cosmetic: the runner threads a stage output to the
		// next stage's env via executor.buildStageEnv, which only stringifies
		// string-typed inputs (SEC-045). A numeric selectedNumber here is a
		// float64 in the merged Outputs, so it was silently dropped and
		// apply-verdict aborted with "selectedNumber is required" on every run —
		// no PR ever received a merge-review label since #381.
		"selectedNumber":  selectedNumberStr,
		"selectedHeadSha": selectedHeadSHA,
		"selectedBaseSha": selectedBaseSHA,
		"siblings":        siblings,
	}, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal sibling context: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "gathered context for %d sibling PR(s) (%d reused from cache, %d fetched fresh)\n",
		len(siblings), reused, len(siblings)-reused)
	return 0
}
