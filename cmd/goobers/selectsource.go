package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/providers"
)

const selectSourceHelp = "Usage: goobers select-source [path]\n\n" +
	"select-source is the decomposition workflow's `select-source` stage\n" +
	"(docs/design/decomposition-workflow.md §3.2, DEC-1). It scans escalated\n" +
	"runs for an unconsumed, non-retryable ISSUE_OVER_SCOPE/NEEDS_DECOMPOSITION\n" +
	"disposition (#415), resolves the oldest eligible one's claimed parent issue,\n" +
	"and — if it is still open, maintainer-approved, not already claimed or\n" +
	"decomposed — claims it in the local claim ledger and writes the immutable\n" +
	"selection artifact to the declared result file. The maintainer approval\n" +
	"label is configured by the required trustLabel input (SEC-047).\n\n" +
	"Exit codes: 0 = a source was selected (or none was eligible — a no-work\n" +
	"result, not an error) / 1 = business error (provider/credential/config\n" +
	"error) / 2 = usage/IO error.\n"

func runSelectSource(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("select-source", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "select-source")
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
	l := layoutFor(root)

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Every work-item call this stage makes — the parent fetch, its comments,
	// and the claim marker — addresses the BACKLOG container, which since
	// personal-gaggle-routing may be a different repository, forge, or account
	// than the routed code repo. Resolved fail-loud (backlogRepositoryRefForStage,
	// not backlogRepoRefForStage): select-source takes ownership of the parent,
	// and silently falling back to the project repository would claim an item in
	// one container while addressing another.
	backlogRepo, err := backlogRepositoryRefForStage(root, repo)
	if err != nil {
		pf(stderr, "error: resolve backlog repository: %v\n", err)
		return 1
	}
	trustLabel := strings.TrimSpace(providerInput("trustLabel", ""))
	if trustLabel == "" {
		pln(stderr, "error: trustLabel is required for decomposition selection (SEC-047)")
		return 1
	}

	// Built for the backlog repository AND authenticated as the backlog
	// connection: the two must never disagree, or a cross-account backlog is
	// reached with a project token that cannot see it (an opaque 404/403).
	issueProvider, err := newBacklogProviderForStage(root, repo, backlogRepo, false,
		withStageProviderCache(), withStageProviderMutations("issue"))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	gaggle := providerGaggle()

	// The authoritative claim scope, resolved BEFORE any claim is taken. A
	// gaggle-owned run must never degrade to a gaggle-scoped key: two gaggles
	// sharing one backlog would then both be able to claim the same parent
	// (§5.3). A standalone invocation has no siblings to contend with and keeps
	// the historical key, exactly as backlog-query does.
	backlogIdentity, identityErr := backlogIdentityForStage(root, repo)
	if identityErr != nil && gaggle != "" {
		pf(stderr, "error: resolve backlog identity: %v\n", identityErr)
		return 1
	}

	leaseDuration := DefaultClaimLease
	if s := providerInput("leaseDuration", ""); s != "" {
		d, perr := time.ParseDuration(s)
		if perr != nil || d <= 0 {
			pf(stderr, "error: invalid leaseDuration %q: must be a positive duration\n", s)
			return 1
		}
		leaseDuration = d
	}

	ctx, cancel := providerCommandContext()
	defer cancel()

	reads, err := readservice.NewOfflineRuns(l)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	candidates, err := decomposition.FindEscalationCandidates(ctx, reads)
	if err != nil {
		return failProviderStage(stderr, "find escalation candidates", err, "selection.json")
	}

	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		pf(stderr, "error: open instance log: %v\n", err)
		return 1
	}
	defer func() { _ = instanceLog.Close() }()

	for _, candidate := range candidates {
		item, getErr := issueProvider.GetWorkItem(ctx, backlogRepo, candidate.ParentID)
		if getErr != nil {
			// The parent may have been deleted/transferred since the source
			// run touched it; skip it rather than fail the whole scan.
			continue
		}
		if !parentEligibleForDecomposition(item, trustLabel) {
			continue
		}
		comments, commentsErr := issueProvider.ListComments(ctx, backlogRepo, item.ID)
		if commentsErr != nil {
			return failProviderStage(stderr, fmt.Sprintf("list comments for parent %s", item.ID), commentsErr, "selection.json")
		}
		if decomposition.HasExistingBatchMarker(commentBodies(comments)) {
			// Already decomposed (or a decomposition is already prepared) —
			// this source run is consumed by the existing batch, not a fresh
			// claim (design doc §2.1).
			continue
		}

		key := selectSourceClaimKey(backlogIdentity, gaggle, backlogRepo, item.ID)
		ok, _, claimErr := claimSelectSourceParent(l.SchedulerDir(), instanceLog, key, runID, workflow, leaseDuration)
		if claimErr != nil {
			return failProviderStage(stderr, fmt.Sprintf("claim parent %s", item.ID), claimErr, "selection.json")
		}
		if !ok {
			// Already claimed by a different run (another decomposition
			// selector, or an unrelated implementation claim) — fail closed
			// on this candidate and keep scanning.
			continue
		}

		digest, digestErr := decomposition.IssueSnapshotDigest(item.ID, item.Title, item.Body, decompositionDigestLabels(item.Labels), item.State)
		if digestErr != nil {
			if releaseErr := releaseSelectSourceParent(l.SchedulerDir(), instanceLog, key, runID); releaseErr != nil {
				pf(stderr, "error: release claim %s after digest failure: %v\n", item.ID, releaseErr)
			}
			return failProviderStage(stderr, "compute issue snapshot digest", digestErr, "selection.json")
		}

		observedRevision := ""
		if item.UpdatedAt != nil {
			observedRevision = item.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		selection := decomposition.Selection{
			Mode:           decomposition.SelectionModeEscalation,
			SourceRunID:    candidate.SourceRunID,
			SourceWorkflow: candidate.SourceWorkflow,
			SourceStage:    candidate.SourceStage,
			ErrorCode:      candidate.ErrorCode,
			ErrorMessage:   candidate.ErrorMessage,
			Parent: decomposition.ParentRef{
				// The parent is recorded in BACKLOG coordinates, not project
				// ones: publish-batch re-derives its publisher repository the
				// same way and refuses a plan whose parent names a different
				// container, so the two must agree on the backlog.
				Provider:         string(backlogRepo.Provider),
				Repository:       repositoryDisplayName(backlogRepo),
				ID:               item.ID,
				ObservedRevision: observedRevision,
			},
			IssueSnapshotDigest: digest,
		}

		data, marshalErr := json.Marshal(selection)
		if marshalErr != nil {
			if releaseErr := releaseSelectSourceParent(l.SchedulerDir(), instanceLog, key, runID); releaseErr != nil {
				pf(stderr, "error: release claim %s after marshal failure: %v\n", item.ID, releaseErr)
			}
			pf(stderr, "error: marshal selection: %v\n", marshalErr)
			return 1
		}
		resultFile := providerInput("resultFile", "selection.json")
		if err := os.WriteFile(resultFile, data, 0o644); err != nil {
			if releaseErr := releaseSelectSourceParent(l.SchedulerDir(), instanceLog, key, runID); releaseErr != nil {
				pf(stderr, "error: release claim %s after write failure: %v\n", item.ID, releaseErr)
			}
			pf(stderr, "error: write %s: %v\n", resultFile, err)
			return 1
		}

		// Provider-visible marker: best-effort mirror of the ledger's
		// (already authoritative) decision, same discipline as backlog-query.
		if _, cerr := issueProvider.ClaimWorkItem(ctx, providers.ClaimWorkItemRequest{Repository: backlogRepo, ID: item.ID, RunID: runID}); cerr != nil {
			pf(stderr, "warning: provider claim marker for %s failed (ledger claim still holds): %v\n", item.ID, cerr)
		}

		pf(stdout, "selected parent %s from source run %s (%s)\n", item.ID, candidate.SourceRunID, candidate.ErrorCode)
		return 0
	}

	return writeSelectSourceNoWork(stdout, stderr, "no eligible decomposition source")
}

// writeSelectSourceNoWork mirrors writeNoWorkResult's shape (executor.OutputNoWork,
// exit 0) but defaults to select-source's own result file name rather than
// backlog-query's claimed-item.json.
func writeSelectSourceNoWork(stdout, stderr io.Writer, reason string) int {
	resultFile := providerInput("resultFile", "selection.json")
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

// selectSourceClaimKey builds the parent claim's ownership key. A resolved
// backlog identity makes it authoritative v3 backlog scope — the same key
// backlog-query's own claims use, which is what makes decomposition's parent
// claim mutually exclusive with an implementation run's claim on that item
// even when the two runs belong to different gaggles sharing one backlog
// (§5.3). Only a standalone, gaggle-less invocation with no resolvable backlog
// falls through to the historical gaggle/provider key.
func selectSourceClaimKey(identity apiv1.BacklogIdentity, gaggle string, backlogRepo providers.RepositoryRef, itemID string) localscheduler.ClaimKey {
	if identity.Validate() == nil {
		return backlogClaimKey(identity, gaggle, itemID)
	}
	return localscheduler.ClaimKey{Gaggle: gaggle, Provider: string(backlogRepo.Provider), ExternalID: itemID}
}

// claimSelectSourceParent and releaseSelectSourceParent mirror backlog-query's
// own scoped/legacy dispatch: ClaimKey.storageKey requires either a backlog
// identity or a complete gaggle+provider pair, so a key with neither must use
// the legacy unscoped Claim/Release.
func selectSourceClaimScoped(key localscheduler.ClaimKey) bool {
	return !key.Backlog.IsZero() || (key.Gaggle != "" && key.Provider != "")
}

func claimSelectSourceParent(schedulerDir string, instanceLog *journal.InstanceLog, key localscheduler.ClaimKey, runID, workflow string, leaseDuration time.Duration) (bool, string, error) {
	var ok bool
	var holder string
	err := withClaimLock(filepath.Join(schedulerDir, claimLockFileName), claimLockOperationSelectSourceClaim, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(schedulerDir, claimLedgerFileName),
			localscheduler.WithInstanceLog(instanceLog),
		)
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		if !selectSourceClaimScoped(key) {
			ok, holder, err = ledger.Claim(key.ExternalID, runID, workflow, leaseDuration)
		} else {
			ok, holder, err = ledger.ClaimScoped(key, runID, workflow, leaseDuration)
		}
		return err
	})
	return ok, holder, err
}

func releaseSelectSourceParent(schedulerDir string, instanceLog *journal.InstanceLog, key localscheduler.ClaimKey, runID string) error {
	return withClaimLock(filepath.Join(schedulerDir, claimLockFileName), claimLockOperationSelectSourceRelease, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(schedulerDir, claimLedgerFileName),
			localscheduler.WithInstanceLog(instanceLog),
		)
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		if !selectSourceClaimScoped(key) {
			return ledger.Release(key.ExternalID, runID)
		}
		return ledger.ReleaseScoped(key, runID)
	})
}

// parentEligibleForDecomposition re-verifies the live parent state
// independently of whatever it looked like when the source run claimed it
// (design doc §2.1's fail-closed list): open, maintainer-approved, and not
// already mid-implementation review.
func parentEligibleForDecomposition(item providers.WorkItem, trustLabel string) bool {
	if item.State != "" && !strings.EqualFold(item.State, "open") {
		return false
	}
	if !item.HasLabel(trustLabel) {
		return false
	}
	if item.HasLabel(inReviewStatusLabel) {
		return false
	}
	return true
}

func commentBodies(comments []providers.Comment) []string {
	bodies := make([]string, len(comments))
	for i, c := range comments {
		bodies[i] = c.Body
	}
	return bodies
}

// decompositionDigestLabels excludes select-source's own provider-visible
// claim marker (LabelClaimed, mirroring the local claim ledger — see
// localscheduler.ClaimLedger's own doc comment) from the issue-snapshot
// digest. Without this, select-source claiming a parent would itself add the
// exact label churn that immediately makes validate-plan see a spurious
// live-parent conflict against its own selector's side effect — precisely
// the "semantically irrelevant claim breadcrumb" design doc §4 says must be
// ignored, not a real content change design-slices needs to react to.
func decompositionDigestLabels(labels []string) []string {
	filtered := make([]string, 0, len(labels))
	for _, label := range labels {
		if label == providers.LabelClaimed {
			continue
		}
		filtered = append(filtered, label)
	}
	return filtered
}

func repositoryDisplayName(repo providers.RepositoryRef) string {
	if repo.Project != "" {
		return repo.Owner + "/" + repo.Project + "/" + repo.Name
	}
	return repo.Owner + "/" + repo.Name
}
