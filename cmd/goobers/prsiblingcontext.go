package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// intersectSorted returns the sorted, de-duplicated set of strings present in
// both a and b — the file-overlap primitive for sibling sequencing (#989).
func intersectSorted(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	inA := make(map[string]bool, len(a))
	for _, s := range a {
		inA[s] = true
	}
	seen := make(map[string]bool)
	var out []string
	for _, s := range b {
		if inA[s] && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func verdictHasIndependentSubstantiveFindingForPR(
	verdict *apiv1.Verdict,
	prNumber int,
	overlappingSiblings []int,
	minSeverity apiv1.Severity,
) bool {
	if verdict == nil {
		return false
	}
	target := strconv.Itoa(prNumber)
	overlapping := make(map[string]bool, len(overlappingSiblings))
	for _, number := range overlappingSiblings {
		overlapping[strconv.Itoa(number)] = true
	}
	for _, finding := range verdict.Findings {
		if !substantiveFindingAppliesToPR(finding, target, minSeverity) {
			continue
		}
		locationRefs := prReferencePattern.FindAllStringSubmatch(finding.Location, -1)
		overlapAttributable := len(locationRefs) > 0
		for _, match := range locationRefs {
			if len(match) < 2 || !overlapping[match[1]] {
				overlapAttributable = false
				break
			}
		}
		if !overlapAttributable {
			return true
		}
	}
	return false
}

// siblingPR is one OTHER open PR's evidence for the holistic review — what
// it touches and its own state, so the reviewer can spot cross-PR
// conflict/drift the in-run reviewer (which sees only one diff) never can
// (issue #359, design doc §3).
type siblingPR struct {
	Number     int      `json:"number"`
	URL        string   `json:"url"`
	Head       string   `json:"head"`
	HeadSHA    string   `json:"headSha"`
	Draft      bool     `json:"draft"`
	Labels     []string `json:"labels,omitempty"`
	CheckState string   `json:"checkState"`
	Files      []string `json:"files"`
	// Overlap is the deterministic set of files this sibling changes that the
	// selected PR also changes (#989): the ground-truth file collision, computed
	// here rather than left for the LLM reviewer to notice. Empty when the two
	// PRs touch disjoint files. Feeds the sequencing classification/backstop
	// (#990/#991) and is surfaced to the reviewer as evidence.
	Overlap []string `json:"overlap,omitempty"`
}

type siblingLifecycleOutcome struct {
	Number          int    `json:"number"`
	Outcome         string `json:"outcome"`
	PreviousHeadSHA string `json:"previousHeadSha"`
	CurrentHeadSHA  string `json:"currentHeadSha"`
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
// head SHA is unchanged since the last gather reuses its cached files.
// Check state is always refreshed because CI can be rerun on the same SHA.
const gatherSiblingContextHelp = "Usage: goobers gather-sibling-context [--no-cache] [--no-verdict-cache] [path]\n\n" +
	"Load the other open PRs' touched files + state as\n" +
	"evidence for the holistic review (a workflow stage, follows\n" +
	"pr-select). authorScope=any permits an outside-headPrefixes selection and\n" +
	"requires pr-select's advisoryMode output. Managed selections retain their\n" +
	"namespace-wide goober sibling set; advisory selections see every open PR.\n" +
	"Requires selectedNumber from Task.InputsFrom. Sibling files are memoized\n" +
	"per head SHA under the instance scheduler dir, while check state is\n" +
	"always refreshed; --no-cache bypasses the file memo entirely (neither\n" +
	"read nor written) to force a fully fresh gather. Separately, this stage\n" +
	"also computes the selected PR's\n" +
	"reviewDigest and checks the PR's own most recent verdict comment for a\n" +
	"matching one (issue #523's verdict-level cache) — a match is emitted as\n" +
	"cachedVerdictJson, letting the runner skip the reviewer gate's LLM call\n" +
	"entirely. For a managed PR, however, a matching fail verdict is marked\n" +
	"stale and not emitted when an operator has cleared goobers:merge-escalated,\n" +
	"so the stage forces a fresh review instead; --no-verdict-cache skips that\n" +
	"lookup, always forcing a fresh review. Exit codes: 0 = context gathered\n" +
	"(possibly empty — no siblings is not an error), 1 = business error,\n" +
	"2 = usage/IO error.\n"

func runGatherSiblingContext(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("gather-sibling-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "gather-sibling-context")
	noCache := fs.Bool("no-cache", false, "bypass the sibling-context cache (debug/remediation escape hatch)")
	noVerdictCache := fs.Bool("no-verdict-cache", false, "skip the verdict-cache lookup, always forcing a fresh review (debug/remediation escape hatch)")
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
	if repo.Provider == providers.ProviderADO {
		return runGatherSiblingContextADO(root, repo, stdout, stderr)
	}
	provider, err := newMergeReviewProviderAs[*providers.GitHubProvider](root, repo, true,
		withStageProviderCapability(capability.GitHubPRWrite),
		withStageProviderCache(),
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

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
	base := providerInput("base", providerBaseBranch())
	headPrefixes := mergeReviewHeadPrefixes()
	authorScope := providerInput("authorScope", authorScopeGoobers)
	if authorScope != authorScopeGoobers && authorScope != authorScopeAny {
		pf(stderr, "error: authorScope input %q must be %q or %q\n", authorScope, authorScopeGoobers, authorScopeAny)
		return 1
	}
	advisoryMode, err := strconv.ParseBool(providerInput("advisoryMode", "false"))
	if err != nil {
		pf(stderr, "error: invalid advisoryMode input: %v\n", err)
		return 1
	}
	managedHeadPrefix := providerBranchNamespace()
	listHeadPrefix := managedHeadPrefix
	if authorScope == authorScopeAny {
		listHeadPrefix = ""
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	expectedAuthorLogin := daemonIdentityAuthorLogin(ctx, root, provider)
	// SkipCheckState: the list is the always-fresh probe (one request), but
	// per-candidate check-state resolution is two more requests per PR. It is
	// resolved below after file-list memoization so same-head CI reruns are
	// reflected in the verdict-cache key.
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: listHeadPrefix, SkipCheckState: true,
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

	var selectedHead, selectedHeadSHA, selectedBaseSHA, selectedAuthor string
	selectedFound := false
	var selectedFiles []string
	var selectedLines int
	var selectedLabels []string
	reused := 0
	siblings := make([]siblingPR, 0, len(prs))
	lifecycleOutcomes := make([]siblingLifecycleOutcome, 0)
	failCheckState := func(number int, err error) int {
		return failProviderStage(stderr, fmt.Sprintf("check state for PR #%d", number), err, "sibling-context.json")
	}
siblingLoop:
	for _, pr := range prs {
		if pr.Number == selectedNumber {
			selectedFound = true
			// Capture the selected PR's OWN current SHAs from this same
			// fresh query — this is what the review gate's Verdict should
			// pin against (design doc §6 D6), not whatever pr-select saw
			// several stages ago.
			selectedHead, selectedHeadSHA, selectedBaseSHA = pr.Head, pr.HeadSHA, pr.BaseSHA
			selectedAuthor = pr.Author
			// Its current labels, for the #1111 scope-drift flag's idempotency.
			selectedLabels = pr.Labels
			// Capture its own changed files too (#989), so overlap against
			// each sibling can be computed deterministically below. Reuse the
			// memo on a SHA match, same as siblings, else fetch once.
			key := strconv.Itoa(pr.Number)
			prior, hit := cached[key]
			hit = hit && prior.HeadSHA == pr.HeadSHA
			if hit {
				selectedFiles = prior.Files
				selectedLines = prior.Lines
			} else {
				files, ferr := provider.PullRequestFiles(ctx, repo, key)
				if ferr != nil {
					return failProviderStage(stderr, fmt.Sprintf("list files for selected PR #%d", pr.Number), ferr, "sibling-context.json")
				}
				selectedFiles = make([]string, 0, len(files))
				for _, f := range files {
					selectedFiles = append(selectedFiles, f.Path)
					selectedLines += f.Additions + f.Deletions
				}
			}
			// Keep its still-valid memo through the save's prune-to-open-set:
			// this PR is a *sibling* from every other run's perspective, and
			// merge-review cycles through selections — evicting here would
			// force the very next run to re-fetch it. Preserve/refresh its
			// files+check-state memo so the capture above isn't wasted.
			if hit {
				next[key] = prior
			} else {
				next[key] = siblingCacheEntry{HeadSHA: pr.HeadSHA, CheckState: prior.CheckState, Files: selectedFiles, Lines: selectedLines}
			}
			continue
		}
		if !advisoryMode && !strings.HasPrefix(pr.Head, managedHeadPrefix) {
			continue
		}
		key := strconv.Itoa(pr.Number)
		prior, hit := cached[key]
		hit = hit && prior.HeadSHA == pr.HeadSHA
		retriedAfterHeadMove := false
		for {
			paths := prior.Files
			lines := prior.Lines
			if !hit {
				files, ferr := provider.PullRequestFiles(ctx, repo, key)
				if ferr != nil {
					return failProviderStage(stderr, fmt.Sprintf("list files for PR #%d", pr.Number), ferr, "sibling-context.json")
				}
				paths = make([]string, 0, len(files))
				lines = 0
				for _, f := range files {
					paths = append(paths, f.Path)
					lines += f.Additions + f.Deletions
				}
			}

			checkState, checkErr := provider.RefCheckState(ctx, repo, pr.HeadSHA)
			if checkErr == nil {
				if hit {
					reused++
				}
				next[key] = siblingCacheEntry{HeadSHA: pr.HeadSHA, CheckState: checkState, Files: paths, Lines: lines}
				siblings = append(siblings, siblingPR{
					Number: pr.Number, URL: pr.URL, Head: pr.Head, HeadSHA: pr.HeadSHA, Draft: pr.Draft,
					Labels: pr.Labels, CheckState: string(checkState), Files: paths,
				})
				break
			}

			refreshed, refreshErr := provider.GetPullRequest(ctx, repo, key)
			if refreshErr != nil || refreshed.Number != pr.Number {
				return failCheckState(pr.Number, checkErr)
			}

			outcome := ""
			switch {
			case refreshed.Merged:
				outcome = "merged"
			case strings.EqualFold(refreshed.State, "closed"):
				outcome = "closed"
			case refreshed.HeadSHA != pr.HeadSHA:
				outcome = "head-moved"
			default:
				return failCheckState(pr.Number, checkErr)
			}
			lifecycleOutcomes = append(lifecycleOutcomes, siblingLifecycleOutcome{
				Number:          pr.Number,
				Outcome:         outcome,
				PreviousHeadSHA: pr.HeadSHA,
				CurrentHeadSHA:  refreshed.HeadSHA,
			})
			if outcome != "head-moved" {
				continue siblingLoop
			}
			if retriedAfterHeadMove {
				continue siblingLoop
			}

			pr = refreshed
			prior = siblingCacheEntry{}
			hit = false
			retriedAfterHeadMove = true
		}
	}

	// Persist before the selected-vanished check: sibling evidence gathered
	// on a run that ends up moot is still valid memo for the next run.
	if !*noCache {
		if err := saveSiblingCache(schedulerDir, next); err != nil {
			pf(stderr, "warning: persist sibling-context cache: %v\n", err)
		}
	}

	if !selectedFound {
		// The selected PR vanished from the eligible list between pr-select
		// and here (merged/closed/retargeted mid-cycle) — nothing to review.
		return writeNoWorkResult(stdout, stderr, "selected PR is no longer open")
	}
	if hasAnyLabel(selectedLabels, []string{noMergeReviewLabel}) {
		return writeNoWorkResult(stdout, stderr, "selected PR opted out of merge-review")
	}
	expectedAdvisoryMode := authorScope == authorScopeAny && !isOwnPullRequest(selectedAuthor, selectedHead, headPrefixes, expectedAuthorLogin)
	if advisoryMode != expectedAdvisoryMode {
		pf(stderr, "error: advisoryMode %t does not match selected PR head %q under authorScope %q and headPrefixes %q\n",
			advisoryMode, selectedHead, authorScope, strings.Join(headPrefixes, ","))
		return 1
	}

	// Scope-drift flag (#1111): this stage already fetched the selected PR's
	// changed files and holds github:pr:write, so it is the natural (zero extra
	// list) place to flag a mega-merge-sized diff for a human before it lands.
	// Best-effort — a flag must never block review, so any error is a warning.
	changedFiles := len(selectedFiles)
	scopeDriftThreshold := defaultScopeDriftThreshold
	if v := providerInput("scopeDriftThreshold", ""); v != "" {
		if n, cerr := strconv.Atoi(v); cerr == nil {
			scopeDriftThreshold = n
		}
	}
	if !advisoryMode {
		if flipped, ferr := flagScopeDrift(ctx, provider, repo, selectedNumber, selectedLabels, changedFiles, scopeDriftThreshold); ferr != nil {
			pf(stderr, "warning: scope-drift flag: %v\n", ferr)
		} else if flipped {
			pf(stdout, "scope-drift: PR #%d changes %d files (threshold %d) — %s goobers:scope-drift\n",
				selectedNumber, changedFiles, scopeDriftThreshold,
				map[bool]string{true: "applied", false: "cleared"}[changedFiles > scopeDriftThreshold])
		}
	}

	// Scope gate (#1313/#1814): a PR meeting or exceeding the threshold on
	// EITHER dimension is barred from merging but remains reviewable. Reuses
	// the same already-fetched files; best-effort, like the advisory flag.
	scopeGateFilesThreshold := defaultScopeGateFilesThreshold
	if v := providerInput("scopeGateFilesThreshold", ""); v != "" {
		if n, cerr := strconv.Atoi(v); cerr == nil {
			scopeGateFilesThreshold = n
		}
	}
	scopeGateLinesThreshold := defaultScopeGateLinesThreshold
	if v := providerInput("scopeGateLinesThreshold", ""); v != "" {
		if n, cerr := strconv.Atoi(v); cerr == nil {
			scopeGateLinesThreshold = n
		}
	}
	scopeGateParked := false
	if !advisoryMode {
		var flipped bool
		scopeGateParked, flipped, err = reconcileScopeGate(
			ctx, provider, repo, selectedNumber, selectedLabels,
			changedFiles, selectedLines, scopeGateFilesThreshold, scopeGateLinesThreshold)
		if err != nil {
			pf(stderr, "warning: scope gate: %v\n", err)
		} else if flipped {
			pf(stdout, "scope-gate: PR #%d changes %d files / %d lines (thresholds %d/%d) — %s goobers:scope-gate\n",
				selectedNumber, changedFiles, selectedLines, scopeGateFilesThreshold, scopeGateLinesThreshold,
				map[bool]string{true: "applied", false: "cleared"}[scopeGateParked])
		}
	}

	// Deterministic file-overlap (#989): the ground-truth set of files each
	// sibling shares with the selected PR, computed here so the sequencing
	// classification/backstop (#990/#991) never depends on the LLM reviewer
	// noticing the collision. overlappingSiblings is the convenience summary
	// those stages consume.
	overlappingSiblings := make([]int, 0)
	overlappingCsv := make([]string, 0)
	for i := range siblings {
		siblings[i].Overlap = intersectSorted(selectedFiles, siblings[i].Files)
		if len(siblings[i].Overlap) > 0 {
			overlappingSiblings = append(overlappingSiblings, siblings[i].Number)
			overlappingCsv = append(overlappingCsv, strconv.Itoa(siblings[i].Number))
		}
	}

	hasSubstantiveFindings := providerInput("hasSubstantiveFindings", "false") == "true"
	if hasSubstantiveFindings && len(overlappingSiblings) > 0 {
		comments, cerr := provider.ListComments(ctx, repo, selectedNumberStr)
		if cerr != nil {
			return failProviderStage(stderr, fmt.Sprintf("list comments on PR #%d", selectedNumber), cerr, "sibling-context.json")
		}
		author, aerr := provider.AuthenticatedLogin(ctx)
		if aerr != nil {
			return failProviderStage(stderr, "resolve merge-review verdict author", aerr, "sibling-context.json")
		}
		if verdict := gatherPRVerdict(comments, author); verdict != nil {
			hasSubstantiveFindings = verdictHasIndependentSubstantiveFindingForPR(
				verdict, selectedNumber, overlappingSiblings, resolveMinSeverity(stderr),
			)
		}
	}

	// Verdict-level cache: the key is the selected PR's own reviewable state
	// (head/base SHAs and gate-relevant labels), NOT the whole sibling set
	// (#1237 — see
	// computeReviewDigest). Check the selected PR's trusted status comment for a
	// matching usable verdict. Any missing key component or lookup problem
	// degrades to a fresh review. Clearing an escalation also invalidates a
	// matching fail verdict: the operator explicitly requested another review.
	reviewDigest := computeReviewDigest(selectedHeadSHA, selectedBaseSHA, selectedLabels)
	var cachedVerdictJSON string
	if reviewDigest == "" {
		pf(stderr, "warning: verdict cache key is incomplete; forcing a fresh review\n")
	} else if !*noVerdictCache {
		cached, cerr := findCachedVerdict(ctx, provider, repo, selectedNumber, reviewDigest, selectedHeadSHA, selectedBaseSHA)
		if cerr != nil {
			pf(stderr, "warning: verdict-cache lookup: %v\n", cerr)
		} else if !advisoryMode && cached != nil && cached.Decision == apiv1.VerdictFail &&
			!hasAnyLabel(selectedLabels, []string{remediationEscalatedLabel}) {
			reason := remediationEscalatedLabel + " was cleared by an operator"
			if err := markMergeReviewVerdictStale(ctx, provider, repo, selectedNumber, reason); err != nil {
				pf(stderr, "warning: could not mark PR #%d's operator-cleared verdict stale: %v\n", selectedNumber, err)
			}
			pf(stdout, "PR #%d: %s — invalidated the standing fail verdict and forcing a fresh review\n", selectedNumber, reason)
		} else if cached != nil && !cachedBlockerVerdictStillApplies(*cached, siblings) {
			// The head/base key still matches, but the cached verdict is a
			// blocked-on-sibling verdict whose named blocker(s) have all resolved
			// (merged/closed/demoted). Reusing it would keep the PR parked behind
			// a block that no longer exists, so force a fresh review (#1237).
			pf(stderr, "info: cached verdict's named blocker(s) have resolved; forcing a fresh review\n")
		} else if cached != nil {
			data, merr := json.Marshal(cached)
			if merr != nil {
				pf(stderr, "warning: marshal cached verdict: %v\n", merr)
			} else {
				cachedVerdictJSON = string(data)
			}
		}
	}

	resultFile := providerInput("resultFile", "sibling-context.json")
	out := map[string]interface{}{
		// selectedNumber is emitted as a STRING (selectedNumberStr, not the
		// parsed int), matching pr-select's "number":"403" and apply-verdict's
		// strconv.Atoi consumer — one type end-to-end (#413). This is
		// load-bearing, not cosmetic: the runner threads a stage output to the
		// next stage's env via executor.buildStageEnv, which only stringifies
		// string-typed inputs (SEC-045). A numeric selectedNumber here is a
		// float64 in the merged Outputs, so it was silently dropped and
		// apply-verdict aborted with "selectedNumber is required" on every run —
		// no PR ever received a merge-review label since #381.
		"selectedNumber":           selectedNumberStr,
		"head":                     selectedHead,
		"base":                     base,
		"hasSubstantiveFindings":   strconv.FormatBool(hasSubstantiveFindings),
		"hasFailingCI":             providerInput("hasFailingCI", "false"),
		"hasSiblingOverlap":        strconv.FormatBool(len(overlappingSiblings) > 0),
		"advisoryMode":             strconv.FormatBool(advisoryMode),
		"selectedHeadSha":          selectedHeadSHA,
		"selectedBaseSha":          selectedBaseSHA,
		"reviewDigest":             reviewDigest,
		"siblings":                 siblings,
		"siblingLifecycleOutcomes": lifecycleOutcomes,
		// overlappingSiblings: PR numbers whose files intersect the selected
		// PR's (#989). Empty slice, not omitted, so a consumer can distinguish
		// "computed, none overlap" from "field absent / older producer".
		"overlappingSiblings": overlappingSiblings,
		// overlappingSiblingsCsv: the same set as a comma-separated scalar
		// string (#990), so the runner's flat-scalar Outputs harvest lifts it
		// for inputsFrom threading to elect-lander/apply-verdict (a []int array
		// is not lifted). Empty string when nothing overlaps.
		"overlappingSiblingsCsv": strings.Join(overlappingCsv, ","),
		// selectedChangedFiles: the selected PR's changed-file count (#1111),
		// emitted as a string for the runner's flat-scalar Outputs harvest — the
		// magnitude the scope-drift flag above acts on, surfaced for observability
		// and for any future gate that wants to branch on it.
		"selectedChangedFiles": strconv.Itoa(changedFiles),
		// selectedChangedLines: the selected PR's total changed-line count
		// (sum of every file's additions+deletions, #1313) — the scope
		// gate's second magnitude, computed from the same PullRequestFiles
		// data selectedChangedFiles already comes from.
		"selectedChangedLines": strconv.Itoa(selectedLines),
		// scopeGateParked: whether reconcileScopeGate currently bars this PR
		// from merging (#1313/#1814). The workflow carries it through review
		// publication to its pre-merge scope gate.
		"scopeGateParked": strconv.FormatBool(scopeGateParked),
	}
	if cachedVerdictJSON != "" {
		// A scalar string (not a nested object) so executor.
		// mergeResultFileOutputs' flat-object merge actually lifts it into
		// Outputs["cachedVerdictJson"] — the runner reads it there directly
		// (evaluateGate), never through a declared inputsFrom edge.
		out["cachedVerdictJson"] = cachedVerdictJSON
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal sibling context: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	cacheNote := "no verdict cache hit"
	if cachedVerdictJSON != "" {
		cacheNote = "verdict cache HIT — reviewer call will be skipped"
	}
	pf(stdout, "gathered context for %d sibling PR(s) (%d reused from cache, %d fetched fresh); %s\n",
		len(siblings), reused, len(siblings)-reused, cacheNote)
	return 0
}

// runGatherSiblingContextADO produces the gather-sibling-context stage output on
// Azure DevOps. The GitHub path's sibling scan leans on surfaces ADO gaps or
// that carry GitHub-only identity semantics (RefCheckState, AuthenticatedLogin,
// the sibling file-overlap and verdict caches, the scope-drift/scope-gate label
// writes) — none of which the single-clean-PR ADO land needs. So on ADO this
// stage resolves only the selected PR's own head/base SHAs (the deterministic
// pin apply-verdict requires via selectedHeadSha/selectedBaseSha) with one
// PollPullRequest and emits an EMPTY sibling set: the review gate then has
// trivial no-sibling evidence and reviews the single diff, keeping the run
// moving. Cross-PR sibling sequencing on ADO is deferred to the ADO merge epic
// (#2061). It never resolves a github:* capability token — the ADO provider
// resolves its own org-scoped auth from instance config.
func runGatherSiblingContextADO(root string, repo providers.RepositoryRef, stdout, stderr io.Writer) int {
	provider, err := newMergeReviewProviderAs[*providers.ADOProvider](root, repo, true)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	selectedNumberStr := providerInput("selectedNumber", "")
	if selectedNumberStr == "" {
		pf(stderr, "error: selectedNumber is required (inputsFrom pr-select's number output)\n")
		return 1
	}
	if _, aerr := strconv.Atoi(selectedNumberStr); aerr != nil {
		pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, aerr)
		return 1
	}
	base := providerInput("base", providerBaseBranch())
	advisoryMode, err := strconv.ParseBool(providerInput("advisoryMode", "false"))
	if err != nil {
		pf(stderr, "error: invalid advisoryMode input: %v\n", err)
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	poll, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: repo, PullID: selectedNumberStr})
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("poll pull request #%s", selectedNumberStr), err, "sibling-context.json")
	}
	if poll.State != "open" || poll.Merged {
		// The selected PR closed/merged/retargeted between pr-select and here —
		// nothing to review, the same disposition as the GitHub
		// selected-vanished path above.
		return writeNoWorkResult(stdout, stderr, "selected PR is no longer open")
	}

	selectedHead := poll.HeadBranch
	if selectedHead == "" {
		selectedHead = providerInput("head", "")
	}
	if base == "" {
		base = poll.BaseBranch
	}

	resultFile := providerInput("resultFile", "sibling-context.json")
	out := map[string]interface{}{
		"selectedNumber":         selectedNumberStr,
		"head":                   selectedHead,
		"base":                   base,
		"hasSubstantiveFindings": providerInput("hasSubstantiveFindings", "false"),
		"hasFailingCI":           providerInput("hasFailingCI", "false"),
		"hasSiblingOverlap":      "false",
		"advisoryMode":           strconv.FormatBool(advisoryMode),
		"selectedHeadSha":        poll.HeadSHA,
		"selectedBaseSha":        poll.BaseSHA,
		"reviewDigest":           computeReviewDigest(poll.HeadSHA, poll.BaseSHA, poll.Labels),
		// Empty slices (not omitted) so a consumer distinguishes "computed, none
		// overlap" from "field absent"; matches the GitHub producer above.
		"siblings":               []siblingPR{},
		"overlappingSiblings":    []int{},
		"overlappingSiblingsCsv": "",
		// Scope drift/gate are GitHub-only label mechanics; report zero/false so
		// the pre-merge scope gate this threads into never parks on ADO.
		"selectedChangedFiles": "0",
		"selectedChangedLines": "0",
		"scopeGateParked":      "false",
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal sibling context: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "gathered context for 0 sibling PR(s) on Azure DevOps (empty sibling set — single-PR review)\n")
	return 0
}
