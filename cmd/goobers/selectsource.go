package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journalclient"
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
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	l := layoutFor(root)

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	trustLabel := strings.TrimSpace(providerInput("trustLabel", ""))
	if trustLabel == "" {
		pln(stderr, "error: trustLabel is required for decomposition selection (SEC-047)")
		return 1
	}

	issueProvider, err := newProviderForStage(root, repo, false, withStageProviderCache(), withStageProviderMutations("issue"))
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

	crossRun, err := stageCrossRunJournal(root, nil)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	candidates, err := crossRun.EscalationCandidates(ctx, journalclient.EscalationCandidatesRequest{RunID: runID, Gaggle: gaggle})
	if err != nil {
		return failProviderStage(stderr, "find escalation candidates", err, "selection.json")
	}

	instanceLog, closeLog, err := claimLedgerJournal(l)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer closeLog()
	ledger, err := openStageClaimLedger(l, withClaimJournal(instanceLog)...)
	if err != nil {
		pf(stderr, "error: open claim ledger: %v\n", err)
		return 1
	}

	for _, candidate := range candidates {
		item, getErr := issueProvider.GetWorkItem(ctx, repo, candidate.ParentID)
		if getErr != nil {
			// The parent may have been deleted/transferred since the source
			// run touched it; skip it rather than fail the whole scan.
			continue
		}
		if !parentEligibleForDecomposition(item, trustLabel) {
			continue
		}
		comments, commentsErr := issueProvider.ListComments(ctx, repo, item.ID)
		if commentsErr != nil {
			return failProviderStage(stderr, fmt.Sprintf("list comments for parent %s", item.ID), commentsErr, "selection.json")
		}
		if decomposition.HasExistingBatchMarker(commentBodies(comments)) {
			// Already decomposed (or a decomposition is already prepared) —
			// this source run is consumed by the existing batch, not a fresh
			// claim (design doc §2.1).
			continue
		}

		key := claimsclient.Key{Gaggle: gaggle, Provider: string(repo.Provider), ExternalID: item.ID}
		ok, _, claimErr := ledger.ClaimScoped(ctx, key, runID, workflow, leaseDuration)
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
			if releaseErr := ledger.ReleaseScoped(ctx, key, runID); releaseErr != nil {
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
				Provider:         string(repo.Provider),
				Repository:       repositoryDisplayName(repo),
				ID:               item.ID,
				ObservedRevision: observedRevision,
			},
			IssueSnapshotDigest: digest,
		}

		data, marshalErr := json.Marshal(selection)
		if marshalErr != nil {
			if releaseErr := ledger.ReleaseScoped(ctx, key, runID); releaseErr != nil {
				pf(stderr, "error: release claim %s after marshal failure: %v\n", item.ID, releaseErr)
			}
			pf(stderr, "error: marshal selection: %v\n", marshalErr)
			return 1
		}
		resultFile := providerInput("resultFile", "selection.json")
		if err := os.WriteFile(resultFile, data, 0o644); err != nil {
			if releaseErr := ledger.ReleaseScoped(ctx, key, runID); releaseErr != nil {
				pf(stderr, "error: release claim %s after write failure: %v\n", item.ID, releaseErr)
			}
			pf(stderr, "error: write %s: %v\n", resultFile, err)
			return 1
		}

		// Provider-visible marker: best-effort mirror of the ledger's
		// (already authoritative) decision, same discipline as backlog-query.
		if _, cerr := issueProvider.ClaimWorkItem(ctx, providers.ClaimWorkItemRequest{Repository: repo, ID: item.ID, RunID: runID}); cerr != nil {
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
