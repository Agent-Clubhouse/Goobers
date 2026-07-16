package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// DefaultClaimLease bounds how long a claimed item stays held before
// localscheduler.ClaimLedger.RecoverExpired (wired into `goobers up`, #131)
// releases it back to the pool. Raised from the original 2h to 6h (issue
// #235, edge 2): a real implementation run is implement -> reviewer gate ->
// make ci -> open-pr -> ci-poll, and ci-poll alone can legitimately run
// close to its own DefaultPollTimeout (30m) *per attempt*, retried — the old
// 2h default was reachable by a real run, not just a theoretical bound,
// which meant RecoverExpired's known liveness-unaware hazard (see its own
// doc comment) could fire on a still-live run in the shipped config, not
// only on a genuinely abandoned one. 6h is comfortably above that realistic
// ceiling while still bounding a genuinely abandoned claim (a crashed run
// whose lease never gets explicitly released) to a reasonable stuck time.
// Overridable via the leaseDuration Task.Input (a time.ParseDuration
// string) — must be positive; see the leaseDuration parsing below and
// localscheduler.ClaimLedger.Claim's own fail-closed check.
const DefaultClaimLease = 6 * time.Hour

func runBacklogQuery(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backlog-query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	claim := fs.Bool("claim", false, "claim the first eligible item (mirrors the claim in the local ledger + provider)")
	release := fs.Bool("release", false, "release this run's claim ledger leases early (issue #234) — no provider access, pure ledger operation")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers backlog-query [--claim | --release] [path]\n\n"+
			"Query the provider for eligible backlog items — labeled with both\n"+
			"trustLabel (SEC-047: required on public repos, since backlog content is\n"+
			"untrusted input otherwise) and requireLabels. With --claim, claims\n"+
			"exactly one via the local claim ledger (source of truth) mirrored to a\n"+
			"provider-visible marker, and writes it to the declared result file.\n"+
			"trustLabel is required with --claim (SEC-047 fails closed, not open) —\n"+
			"a plain list (no --claim) does not require it.\n\n"+
			"With --release, releases every claim this run holds in the local ledger\n"+
			"(issue #234: a workflow that only reads/labels an item, never opening a\n"+
			"PR or closing the issue — e.g. backlog-curation — must release its own\n"+
			"claim explicitly, since issue-close-out's release is reached only by the\n"+
			"implementation workflow). Idempotent: releasing claims this run does not\n"+
			"hold (already released, e.g. re-run after a crash) is a no-op success, not\n"+
			"an error. --claim and --release are mutually exclusive.\n\n"+
			"Exit codes: 0 = eligible item found (and claimed, if --claim) / released\n"+
			"(--release), 1 = business error (no eligible/claimable item, missing\n"+
			"trustLabel with --claim, config/credential/provider error), 2 =\n"+
			"usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *claim && *release {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	if *release {
		return runBacklogQueryRelease(root, stdout, stderr)
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))

	trustLabel := providerInput("trustLabel", "")
	requireLabel := providerInput("requireLabels", "")
	excludeLabel := providerInput("excludeLabels", "")

	// maxItems caps how many eligible items one --claim run claims (#236): it was
	// a dead input everywhere (the query hardcoded a limit and --claim took
	// exactly one), so a documented input was silently ignored — the #130 class
	// of gap. Default 1 (the single-item implementation shape). The provider
	// query scans at least 20 so eligibility filtering has candidates even when
	// maxItems is small (preserving the pre-#236 scan breadth).
	maxItems := 1
	if s := providerInput("maxItems", ""); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil || n < 1 {
			pf(stderr, "error: invalid maxItems %q (want a positive integer)\n", s)
			return 1
		}
		maxItems = n
	}
	queryLimit := maxItems
	if queryLimit < 20 {
		queryLimit = 20
	}

	// SEC-047 fails CLOSED, not open: an empty trustLabel must refuse to
	// claim, not silently skip the trust check and claim anything eligible
	// by requireLabels alone — backlog content on a public repo is untrusted
	// input, and claiming is the mutating, consequential action (it starts
	// implementation work). A read-only list (no --claim) is informational,
	// so it's not gated the same way.
	if *claim && trustLabel == "" {
		pln(stderr, "error: trustLabel is required to claim (SEC-047: backlog content is untrusted input on a public repo) — declare inputs.trustLabel")
		return 1
	}

	ctx := context.Background()
	labels := make([]string, 0, 2)
	if trustLabel != "" {
		labels = append(labels, trustLabel)
	}
	if requireLabel != "" {
		labels = append(labels, requireLabel)
	}
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repo,
		Labels:     labels,
		State:      "open",
		Limit:      queryLimit,
	})
	if err != nil {
		return failProviderStage(stderr, "list work items", err, "claimed-item.json")
	}

	// Re-verify eligibility in code (SEC-047: backlog content is untrusted
	// input on a public repo) rather than trusting the provider query's
	// labels filter alone — a defense-in-depth check, not a redundant one.
	var eligible []providers.WorkItem
	for _, item := range items {
		if trustLabel != "" && !item.HasLabel(trustLabel) {
			continue
		}
		if requireLabel != "" && !item.HasLabel(requireLabel) {
			continue
		}
		if excludeLabel != "" && item.HasLabel(excludeLabel) {
			continue
		}
		eligible = append(eligible, item)
	}

	// Open-PR eligibility backstop (#414 design point 2): excludeLabels alone
	// depends on a label write at PR-open time (implementation.yaml's
	// goobers/status:in-review) that can be missed or, after close-out,
	// removed without the issue ever actually closing (issue-close-out's
	// status=in-review keeps the issue open until the merge event). Without
	// this, a completed rung's issue can look eligible again and get
	// re-claimed into a duplicate PR. Best-effort on capability: only runs
	// when the stage actually declares github:pr:write (implementation.yaml
	// and backlog-curation.yaml both do); a stage that hasn't opted in gets
	// exactly the pre-#414 label-only behavior, not a hard failure — this is
	// a backstop on top of the label check above, not a replacement for it.
	if _, err := providerToken(capability.GitHubPRWrite); err == nil {
		openIssues, err := openPRIssueNumbers(ctx, provider, repo)
		if err != nil {
			return failProviderStage(stderr, "list open pull requests", err, "claimed-item.json")
		}
		backstopped := eligible[:0]
		for _, item := range eligible {
			if openIssues[item.ID] {
				continue
			}
			backstopped = append(backstopped, item)
		}
		eligible = backstopped
	}
	// Claim order was an accident of whichever sort order the provider's
	// List endpoint happens to default to (#350) — GitHub's is undocumented
	// desc-by-created (newest-first), the exact opposite of the README's
	// assumed "natural claim order is ~ascending issue #". Sorting
	// client-side, provider-independent, pins a deterministic FIFO default
	// (oldest-filed-first, the starvation-safe choice) so a future provider
	// API change — or a provider whose own default happens to differ from
	// GitHub's — can't silently flip claim order again. A fuller
	// configurable priority mechanism (native-field or label-list ranking,
	// configurable tie-break) is tracked separately; this is the
	// unconditional baseline every claim order now starts from.
	sortEligibleFIFO(eligible)

	if !*claim {
		if len(eligible) == 0 {
			pln(stdout, "no eligible items")
			return 0
		}
		for _, item := range eligible {
			pf(stdout, "%s\t%s\n", item.ID, item.Title)
		}
		return 0
	}

	if len(eligible) == 0 {
		return writeNoWorkResult(stdout, stderr, "no eligible item to claim")
	}

	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	leaseDuration := DefaultClaimLease
	if s := providerInput("leaseDuration", ""); s != "" {
		d, perr := time.ParseDuration(s)
		if perr != nil {
			pf(stderr, "error: invalid leaseDuration %q: %v\n", s, perr)
			return 1
		}
		// Fail closed here too, not just in ClaimLedger.Claim (issue #235,
		// edge 1): a non-positive duration is a workflow-authoring mistake,
		// not a business condition — catching it before ever reaching the
		// ledger gives a caller-facing, actionable error instead of a claim
		// silently having no exclusivity.
		if d <= 0 {
			pf(stderr, "error: invalid leaseDuration %q: must be positive\n", s)
			return 1
		}
		leaseDuration = d
	}

	l := layoutFor(root)
	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		pf(stderr, "error: open instance log: %v\n", err)
		return 1
	}
	defer func() { _ = instanceLog.Close() }()

	// Claim up to maxItems eligible items under this run (#236): curation runs a
	// batch (maxItems 20), implementation a single item (maxItems 1). All claims
	// share this run's id; each item gets its own ledger entry.
	var claimed []providers.WorkItem
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	err = withClaimLock(lockPath, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName), localscheduler.WithInstanceLog(instanceLog))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		for i := range eligible {
			if len(claimed) >= maxItems {
				break
			}
			item := eligible[i]
			ok, _, cerr := ledger.Claim(item.ID, runID, workflow, leaseDuration)
			if cerr != nil {
				return fmt.Errorf("claim %s in ledger: %w", item.ID, cerr)
			}
			if ok {
				claimed = append(claimed, item)
			}
		}
		return nil
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Every eligible item is already claimed by another run — a routine no-work
	// tick (#233), not an error: exit 0 with the structured noWork result the
	// runner short-circuits on, rather than the old return 1. Batch-aware len
	// check (#236) replaces #274's pointer-nil check.
	if len(claimed) == 0 {
		return writeNoWorkResult(stdout, stderr, "every eligible item is already claimed by another run")
	}

	// Provider-visible marker per claimed item: best-effort mirror of the
	// ledger's (already authoritative, per localscheduler.ClaimLedger's doc)
	// decision, for human visibility on the provider. A failure here does not
	// undo the ledger claim — the ledger, not this marker, is the source of truth.
	for i := range claimed {
		if _, err := provider.ClaimWorkItem(ctx, providers.ClaimWorkItemRequest{Repository: repo, ID: claimed[i].ID, RunID: runID}); err != nil {
			pf(stderr, "warning: provider claim marker for %s failed (ledger claim still holds): %v\n", claimed[i].ID, err)
		}
	}

	// Result-file shape follows the workflow's cardinality: a single-item run
	// (maxItems 1, implementation) writes the claimed WorkItem as an object so
	// its scalar fields (id/title) merge into the stage's journaled Outputs
	// (open-pr's #241 issue linkage reads them); a batch run (maxItems >1,
	// curation) writes the array the curator persona expects. The stage lifts
	// this file into an artifact only when the workflow declares the resultFile
	// input — curation's #236 fix adds it so the batch actually reaches curate.
	resultFile := providerInput("resultFile", "claimed-item.json")
	var data []byte
	if maxItems == 1 {
		data, err = json.Marshal(claimed[0])
	} else {
		data, err = json.Marshal(claimed)
	}
	if err != nil {
		pf(stderr, "error: marshal claimed item(s): %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	if len(claimed) == 1 {
		pf(stdout, "claimed %s: %s\n", claimed[0].ID, claimed[0].Title)
	} else {
		pf(stdout, "claimed %d items\n", len(claimed))
	}
	return 0
}

// openPRIssueNumbers returns the set of issue numbers already referenced by
// an open goober-authored PR's closing keywords (Fixes/Closes/Resolves #N —
// the same convention `goobers open-pr` writes and `goobers post-merge`
// already parses at merge time via closingIssueNumbers, postmerge.go) — the
// open-PR eligibility backstop (#414 design point 2). One ListPullRequests
// call, not one per candidate: GitHub's list-pulls response already carries
// each PR's body (PullRequestSummary.Body), so no second round-trip per PR
// is needed either.
func openPRIssueNumbers(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef) (map[string]bool, error) {
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{Repository: repo, HeadPrefix: "goobers/"})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(prs))
	for _, pr := range prs {
		for _, id := range closingIssueNumbers(pr.Body) {
			out[id] = true
		}
	}
	return out, nil
}

// sortEligibleFIFO orders items ascending by numeric ID in place — both
// GitHubProvider and ADOProvider mint WorkItem.ID as strconv.Itoa of the
// issue/work-item number, which is monotonically increasing with creation
// order, so this is exactly "oldest filed first" without needing a
// dedicated CreatedAt field or per-provider sort-parameter wiring (#350).
// A non-numeric ID (a future provider whose IDs don't work this way) falls
// back to a stable lexical compare rather than leaving that item's relative
// position to whatever order the provider happened to return it in.
func sortEligibleFIFO(items []providers.WorkItem) {
	sort.SliceStable(items, func(i, j int) bool {
		ni, iOK := parseWorkItemID(items[i].ID)
		nj, jOK := parseWorkItemID(items[j].ID)
		if iOK && jOK {
			return ni < nj
		}
		return items[i].ID < items[j].ID
	})
}

func parseWorkItemID(id string) (int64, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	return n, err == nil
}

// runBacklogQueryRelease implements `backlog-query --release` (issue #234):
// an explicit, deterministic-path release of every claim this run holds, for a
// workflow whose consuming stage neither opens a PR nor closes the issue
// (backlog-curation's curate: it triages/labels the item and stops) — the
// only existing release path, issue-close-out, is reached solely by the
// implementation workflow after it opens a PR, so a curation run's claim was
// otherwise held for the full lease (up to 6h now, was 2h — #235) even
// though curation's own work on the item finished immediately.
//
// No provider/credential access at all — this is a pure ledger operation,
// unlike --claim (which needs a token to list/mirror against the provider).
// That keeps a curation workflow's release stage free of any capability
// declaration, and keeps ledger mutations on the deterministic path per the
// issue's own fix-shape guidance.
func runBacklogQueryRelease(root string, stdout, stderr io.Writer) int {
	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	l := layoutFor(root)
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	var released []string
	err = withClaimLock(lockPath, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		// ForRunAll + Release, not a blind Release-by-guess: idempotency (issue
		// #234's acceptance criterion) requires distinguishing "this run
		// holds nothing to release" (a no-op success — already released, or
		// a crash-resume of this same stage) from an actual release, without
		// needing the caller to already know the item id. Release itself is
		// already a no-op on an unheld item (its own doc comment), so this
		// is belt-and-suspenders for the "nothing to report" stdout case
		// below, not required for correctness.
		for _, entry := range ledger.ForRunAll(runID) {
			if rerr := ledger.Release(entry.ItemID, runID); rerr != nil {
				return fmt.Errorf("release %s in ledger: %w", entry.ItemID, rerr)
			}
			released = append(released, entry.ItemID)
		}
		return nil
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	if len(released) == 0 {
		pln(stdout, "nothing to release: run holds no claim")
		return 0
	}
	pf(stdout, "released %s\n", strings.Join(released, ", "))
	return 0
}

// writeNoWorkResult is `--claim`'s clean, structured "nothing to do this
// tick" outcome (issue #233): an empty eligible set, or every eligible item
// already claimed by another run, is a routine steady state — the same
// backlog-curation.yaml doc comment's own "re-running is a no-op" contract
// — not an error. Exit 0, with the declared result file (the same
// resultFile convention the successful-claim path uses) carrying
// executor.OutputNoWork=true so internal/executor/shell.go reports
// apiv1.ResultNoWork instead of ResultSuccess, and the runner short-circuits
// to a clean PhaseCompleted without ever invoking a downstream agentic
// stage with no subject (internal/runner's taskOutcome). A genuine
// provider/credential/list error is NOT routed through here — those return
// 1 from their own call sites above, unchanged, so a real outage still
// fails the run loudly (the acceptance criteria's negative control).
func writeNoWorkResult(stdout, stderr io.Writer, reason string) int {
	resultFile := providerInput("resultFile", "claimed-item.json")
	data, err := json.Marshal(map[string]interface{}{"claimed": false, executor.OutputNoWork: true})
	if err != nil {
		pf(stderr, "error: marshal no-work result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "no work: %s\n", reason)
	return 0
}
