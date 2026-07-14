package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/capability"
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
	release := fs.Bool("release", false, "release this run's claim ledger lease early (issue #234) — no provider access, pure ledger operation")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers backlog-query [--claim | --release] [path]\n\n"+
			"Query the provider for eligible backlog items — labeled with both\n"+
			"trustLabel (SEC-047: required on public repos, since backlog content is\n"+
			"untrusted input otherwise) and requireLabels. With --claim, claims\n"+
			"exactly one via the local claim ledger (source of truth) mirrored to a\n"+
			"provider-visible marker, and writes it to the declared result file.\n"+
			"trustLabel is required with --claim (SEC-047 fails closed, not open) —\n"+
			"a plain list (no --claim) does not require it.\n\n"+
			"With --release, releases the claim this run holds in the local ledger\n"+
			"(issue #234: a workflow that only reads/labels an item, never opening a\n"+
			"PR or closing the issue — e.g. backlog-curation — must release its own\n"+
			"claim explicitly, since issue-close-out's release is reached only by the\n"+
			"implementation workflow). Idempotent: releasing a claim this run does not\n"+
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
		Limit:      20,
	})
	if err != nil {
		pf(stderr, "error: list work items: %v\n", err)
		return 1
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
		pln(stderr, "error: no eligible item to claim")
		return 1
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

	var claimed *providers.WorkItem
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	err = withClaimLock(lockPath, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName), localscheduler.WithInstanceLog(instanceLog))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		for i := range eligible {
			item := eligible[i]
			ok, _, cerr := ledger.Claim(item.ID, runID, workflow, leaseDuration)
			if cerr != nil {
				return fmt.Errorf("claim %s in ledger: %w", item.ID, cerr)
			}
			if ok {
				claimed = &item
				return nil
			}
		}
		return nil
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if claimed == nil {
		pln(stderr, "error: every eligible item is already claimed by another run")
		return 1
	}

	// Provider-visible marker: best-effort mirror of the ledger's (already
	// authoritative, per localscheduler.ClaimLedger's doc) decision, for
	// human visibility on the provider. A failure here does not undo the
	// ledger claim — the ledger, not this marker, is the source of truth.
	if _, err := provider.ClaimWorkItem(ctx, providers.ClaimWorkItemRequest{Repository: repo, ID: claimed.ID, RunID: runID}); err != nil {
		pf(stderr, "warning: provider claim marker failed (ledger claim still holds): %v\n", err)
	}

	resultFile := providerInput("resultFile", "claimed-item.json")
	data, err := json.Marshal(claimed)
	if err != nil {
		pf(stderr, "error: marshal claimed item: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "claimed %s: %s\n", claimed.ID, claimed.Title)
	return 0
}

// runBacklogQueryRelease implements `backlog-query --release` (issue #234):
// an explicit, deterministic-path release of the claim this run holds, for a
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
	var released string
	err = withClaimLock(lockPath, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		// ForRun + Release, not a blind Release-by-guess: idempotency (issue
		// #234's acceptance criterion) requires distinguishing "this run
		// holds nothing to release" (a no-op success — already released, or
		// a crash-resume of this same stage) from an actual release, without
		// needing the caller to already know the item id. Release itself is
		// already a no-op on an unheld item (its own doc comment), so this
		// is belt-and-suspenders for the "nothing to report" stdout case
		// below, not required for correctness.
		entry, ok := ledger.ForRun(runID)
		if !ok {
			return nil
		}
		if rerr := ledger.Release(entry.ItemID, runID); rerr != nil {
			return fmt.Errorf("release %s in ledger: %w", entry.ItemID, rerr)
		}
		released = entry.ItemID
		return nil
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	if released == "" {
		pln(stdout, "nothing to release: run holds no claim")
		return 0
	}
	pf(stdout, "released %s\n", released)
	return 0
}
