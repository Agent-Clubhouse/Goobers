package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/providers"
)

// issueCloseOutProvider is the provider surface issue-close-out needs: resolve
// the run's PR to link it (FindPullRequestByBranch), mirror the terminal
// processing status (UpdateWorkItemStatus), and the label/comment edits for a
// park (needs-human or needs-remediation) and claim-marker release
// (UpdateWorkItem). Both the GitHub and ADO providers satisfy it, so
// close-out runs against either backend.
type issueCloseOutProvider interface {
	FindPullRequestByBranch(context.Context, providers.RepositoryRef, string, string) (providers.PullRequestResult, bool, error)
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	UpdateWorkItemStatus(context.Context, providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error)
}

type pullRequestReader interface {
	GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error)
}

const issueCloseOutNeedsHuman providers.WorkItemStatus = "needs-human"

// issueCloseOutNeedsRemediation parks an issue the same way issueCloseOutNeedsHuman
// does (swap goobers:ready for a park label, release the claim) but for a
// mechanical failure — a repass-budget exhaustion, an identical-diff loop, an
// infrastructure/executor failure, or a CI-poll timeout — that needs someone
// to act, not a policy decision (#2028). Unlike needs-human it never gets the
// configured human assignee (needshumanrouting.go's withNeedsHumanAssignee
// only fires for LabelNeedsHuman) and it reuses cmd/goobers/postmerge.go's
// needsRemediationLabel, the same label PR-lifecycle remediation already uses.
const issueCloseOutNeedsRemediation providers.WorkItemStatus = "needs-remediation"

// issueCloseOutParkStatuses lists every status that parks the driving issue
// (swaps goobers:ready for a park label) rather than mirroring a processing
// status via UpdateWorkItemStatus. Shared by issueCloseOutStatus's validation
// and runIssueCloseOut's branch so the two can't drift.
var issueCloseOutParkStatuses = []providers.WorkItemStatus{issueCloseOutNeedsHuman, issueCloseOutNeedsRemediation}

func issueCloseOutIsParkStatus(status providers.WorkItemStatus) bool {
	for _, s := range issueCloseOutParkStatuses {
		if status == s {
			return true
		}
	}
	return false
}

func validateIssueCloseOutParkComment(status providers.WorkItemStatus, comment string) error {
	if status == issueCloseOutNeedsHuman && !strings.HasSuffix(strings.TrimSpace(comment), "?") {
		return errors.New("needs-human parking comment must end with the exact question requiring a human decision")
	}
	return nil
}

// issueCloseOutStatus resolves the "status" Task.Input to the WorkItemStatus
// this stage sets, defaulting to WorkItemStatusDone for backward
// compatibility with any workflow that never declares it. Issue #361/#355:
// under the merge-review loop, the work isn't done until the PR merges, so
// `implementation`'s workflow now declares status=in-review here instead —
// only `goobers post-merge` (run by merge-review, at the actual merge event)
// advances the issue to done.
func issueCloseOutStatus(raw string) (providers.WorkItemStatus, error) {
	switch providers.WorkItemStatus(raw) {
	case "":
		return providers.WorkItemStatusDone, nil
	case providers.WorkItemStatusDone, providers.WorkItemStatusInReview:
		return providers.WorkItemStatus(raw), nil
	default:
		if issueCloseOutIsParkStatus(providers.WorkItemStatus(raw)) {
			return providers.WorkItemStatus(raw), nil
		}
		return "", fmt.Errorf("unsupported status %q (want %q, %q, %q, or %q)",
			raw, providers.WorkItemStatusDone, providers.WorkItemStatusInReview, issueCloseOutNeedsHuman, issueCloseOutNeedsRemediation)
	}
}

func issueCloseOutJournal(root, runID string) (journalclient.Reader, error) {
	return stageRunJournal(root, runID)
}

func issueCloseOutReason(root, runID, gateName string) (string, error) {
	reader, err := issueCloseOutJournal(root, runID)
	if err != nil {
		return "", err
	}
	events, err := reader.Events()
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Type {
		case journal.EventGateEvaluated:
			if gateName != "" && event.Gate != gateName {
				continue
			}
			if event.Ref != nil {
				data, err := reader.ArtifactBytes(*event.Ref)
				if err != nil {
					return "", fmt.Errorf("read verdict for gate %q: %w", event.Gate, err)
				}
				var verdict apiv1.Verdict
				if err := json.Unmarshal(data, &verdict); err != nil {
					return "", fmt.Errorf("parse verdict for gate %q: %w", event.Gate, err)
				}
				reason := strings.TrimSpace(verdict.Summary)
				if verdict.Decision == apiv1.VerdictFail {
					reason = strings.TrimSpace(verdict.Rationale)
				}
				if reason == "" {
					reason = strings.TrimSpace(verdict.Rationale)
				}
				if reason == "" {
					reason = strings.TrimSpace(verdict.Summary)
				}
				if reason != "" {
					return reason, nil
				}
			}
			if escalated, _ := event.Runner["escalated"].(bool); escalated {
				if attempt, ok := event.Runner["repassAttempt"].(float64); ok {
					return fmt.Sprintf("gate %s escalated after outcome %s exhausted the repass budget at attempt %.0f", event.Gate, event.Verdict, attempt), nil
				}
				return fmt.Sprintf("gate %s escalated after outcome %s exhausted the repass budget", event.Gate, event.Verdict), nil
			}
			return fmt.Sprintf("gate %s returned terminal outcome %s", event.Gate, event.Verdict), nil
		case journal.EventStageFinished:
			if gateName != "" || event.Status != string(apiv1.ResultFailure) || event.Error == nil {
				continue
			}
			if event.AttemptClass == journal.AttemptInfra && event.Error.Code == "interrupted" {
				continue
			}
			if reason := strings.TrimSpace(event.Error.Message); reason != "" {
				return reason, nil
			}
			if code := strings.TrimSpace(event.Error.Code); code != "" {
				return code, nil
			}
		}
	}
	if gateName == "" {
		return "", fmt.Errorf("no terminal gate or failed task reason found")
	}
	return "", fmt.Errorf("no verdict found for gate %q", gateName)
}

// issueCloseOutReviewVerdict returns the last reviewer verdict journaled for
// the run, with the gate that produced it. #3564: the concrete verdict that
// drove a repass-exhaustion escalation lived only in a content-addressed run
// artifact, so a human triaging the parked issue saw a generic comment and had
// to spelunk events.jsonl to learn what was actually wrong.
func issueCloseOutReviewVerdict(root, runID string) (apiv1.Verdict, string, bool, error) {
	reader, err := issueCloseOutJournal(root, runID)
	if err != nil {
		if errors.Is(err, journalclient.ErrRunNotFound) {
			return apiv1.Verdict{}, "", false, nil
		}
		return apiv1.Verdict{}, "", false, err
	}
	events, err := reader.Events()
	if err != nil {
		return apiv1.Verdict{}, "", false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventGateEvaluated || event.Ref == nil {
			continue
		}
		data, err := reader.ArtifactBytes(*event.Ref)
		if err != nil {
			return apiv1.Verdict{}, "", false, fmt.Errorf("read verdict for gate %q: %w", event.Gate, err)
		}
		var verdict apiv1.Verdict
		if err := json.Unmarshal(data, &verdict); err != nil {
			return apiv1.Verdict{}, "", false, fmt.Errorf("parse verdict for gate %q: %w", event.Gate, err)
		}
		if len(verdict.Findings) == 0 && strings.TrimSpace(verdict.Rationale) == "" &&
			strings.TrimSpace(verdict.Summary) == "" {
			continue
		}
		return verdict, event.Gate, true, nil
	}
	return apiv1.Verdict{}, "", false, nil
}

// issueCloseOutVerdictDetail renders the reviewer verdict a park comment
// embeds: decision, rationale, and every finding's severity, message, and
// location, plus the run id for traceability (#3564).
func issueCloseOutVerdictDetail(verdict apiv1.Verdict, gateName, runID string) string {
	var b strings.Builder
	b.WriteString("\n\n---\n\n**Last review verdict**")
	var qualifiers []string
	if gateName != "" {
		qualifiers = append(qualifiers, fmt.Sprintf("gate `%s`", gateName))
	}
	if decision := strings.TrimSpace(string(verdict.Decision)); decision != "" {
		qualifiers = append(qualifiers, fmt.Sprintf("decision `%s`", decision))
	}
	if runID != "" {
		qualifiers = append(qualifiers, fmt.Sprintf("run `%s`", runID))
	}
	if len(qualifiers) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(qualifiers, ", "))
	}
	b.WriteString("\n")
	if rationale := strings.TrimSpace(verdict.Rationale); rationale != "" {
		fmt.Fprintf(&b, "\n%s\n", rationale)
	} else if summary := strings.TrimSpace(verdict.Summary); summary != "" {
		fmt.Fprintf(&b, "\n%s\n", summary)
	}
	if len(verdict.Findings) > 0 {
		b.WriteString("\nFindings:\n\n")
		for _, finding := range verdict.Findings {
			severity := strings.TrimSpace(string(finding.Severity))
			if severity == "" {
				severity = "finding"
			}
			fmt.Fprintf(&b, "- **%s:** %s", severity, strings.TrimSpace(finding.Message))
			if location := strings.TrimSpace(finding.Location); location != "" {
				fmt.Fprintf(&b, " (`%s`)", location)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func issueCloseOutDuplicateEscalation(root, runID string) (implementationEscalationState, bool, error) {
	reader, err := issueCloseOutJournal(root, runID)
	if err != nil {
		if errors.Is(err, journalclient.ErrRunNotFound) {
			return implementationEscalationState{}, false, nil
		}
		return implementationEscalationState{}, false, err
	}
	events, err := reader.Events()
	if err != nil {
		return implementationEscalationState{}, false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventGateEvaluated {
			continue
		}
		duplicate, _ := event.Runner["duplicateDiff"].(bool)
		digest, _ := event.Runner["diffDigest"].(string)
		if !duplicate || digest == "" {
			continue
		}
		reason, err := issueCloseOutReason(root, runID, event.Gate)
		if err != nil {
			return implementationEscalationState{}, false, err
		}
		cause, _ := event.Runner["repassCause"].(map[string]any)
		if len(cause) != 0 {
			data, err := json.Marshal(cause)
			if err != nil {
				return implementationEscalationState{}, false, fmt.Errorf("marshal repass cause: %w", err)
			}
			var repassCause gate.RepassCause
			if err := json.Unmarshal(data, &repassCause); err != nil {
				return implementationEscalationState{}, false, fmt.Errorf("parse repass cause: %w", err)
			}
			reason = repassCause.String() + "; the implementer produced no change in response"
		}
		return implementationEscalationState{DiffDigest: digest, Reason: reason, Cause: cause}, true, nil
	}
	return implementationEscalationState{}, false, nil
}

const issueCloseOutHelp = "Usage: goobers issue-close-out [path]\n\n" +
	"Comment on the issue this run claimed, linking its PR, and mark it done;\n" +
	"status=in-review leaves it open for merge-review. status=needs-human parks\n" +
	"it by replacing goobers:ready with goobers:needs-human (a decision only a\n" +
	"human can make); status=needs-remediation parks it with goobers:needs-\n" +
	"remediation instead (a mechanical failure needing a fix, not a decision —\n" +
	"#2028). Release the claim ledger lease early rather than waiting for it to\n" +
	"expire. Exit codes: 0 = done, 1 = business error, 2 = usage/IO error.\n"

const (
	implementationInReviewCommentPrefix = "Implementation complete: "
	implementationInReviewCommentSuffix = " is open for merge-review."
)

func implementationInReviewComment(prURL string) string {
	return implementationInReviewCommentPrefix + prURL + implementationInReviewCommentSuffix
}

func runIssueCloseOut(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("issue-close-out", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "issue-close-out")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	stageProvider, err := newProviderForStage(root, repo, false, withStageProviderMutations("issue"))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider, ok := stageProvider.(issueCloseOutProvider)
	if !ok {
		pf(stderr, "error: issue-close-out does not support repository provider %q\n", repo.Provider)
		return 1
	}
	// Work items (the claimed PBI) live in the backlog project on ADO, not the
	// routed code repo whose branch/PR this stage links; address them there.
	backlogRepo := backlogRepoRefForStage(root, repo)

	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	l := layoutFor(root)
	ledger, err := openStageClaimLedger(l)
	if err != nil {
		pf(stderr, "error: open claim ledger: %v\n", err)
		return 1
	}
	var claim claimsclient.Entry
	var claimHeld bool
	err = ledger.Locked(claimContext(), claimLockOperationCloseOutLookup, func(tx claimsclient.Ledger) error {
		// The run's one claimed item (#241; implementation claims at most
		// one per run) — the first of ForRunAll's item-ordered entries, where
		// the ledger's ForRun answered an unspecified one of them.
		entries, lerr := tx.ForRunAll(claimContext(), runID)
		if lerr != nil {
			return fmt.Errorf("read this run's claims: %w", lerr)
		}
		if len(entries) > 0 {
			claim = entries[0]
			claimHeld = true
		}
		return nil
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if !claimHeld {
		// Resume-idempotency (#241): close-out RELEASES the claim as its very
		// last step, so an absent ledger entry means a prior attempt of this
		// stage already ran through the comment + mark-done + release. A crash
		// after the release but before stage.finished is journaled would
		// otherwise re-run close-out here, find no live claim, and fail the run
		// at its final stage after all real work succeeded. Treat an
		// already-released claim as done and succeed as a no-op so the run
		// terminates completed.
		pf(stdout, "run %s: claim already released by a prior close-out; nothing to do\n", runID)
		return 0
	}

	status, err := issueCloseOutStatus(providerInput("status", ""))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	comment := providerInput("comment", "")
	reason := strings.TrimSpace(providerInput("reason", ""))
	if reason != "" {
		comment = reason + "\n\n" + comment
	}
	if issueCloseOutIsParkStatus(status) {
		// #2028: the comment prefix names the disposition — a genuine
		// needs-human park is framed as awaiting a human's review; a
		// needs-remediation park is framed as a mechanical failure awaiting a
		// fix, since no policy decision is actually pending on it.
		parkPrefix := "Implementation parked for human review: "
		if status == issueCloseOutNeedsRemediation {
			parkPrefix = "Implementation parked for remediation: "
		}
		if comment == "" {
			gateName := providerInput("reasonFromGate", "")
			reason, err := issueCloseOutReason(root, runID, gateName)
			if err != nil {
				pf(stderr, "error: resolve parking reason: %v\n", err)
				return 1
			}
			comment = parkPrefix + reason
		}
		if err := validateIssueCloseOutParkComment(status, comment); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		escalation, duplicate, err := issueCloseOutDuplicateEscalation(root, runID)
		if err != nil {
			pf(stderr, "error: resolve duplicate-diff escalation: %v\n", err)
			return 1
		}
		if duplicate {
			comment = parkPrefix + escalation.Reason
		}
		if duplicate && (repo.Provider == providers.ProviderGitHub || repo.Provider == providers.ProviderGitea) {
			head := providerInput("head", providers.BranchNameIn(providerBranchNamespace(), workflow, runID))
			base := providerInput("base", providerBaseBranch())
			pr, found, err := provider.FindPullRequestByBranch(ctx, repo, head, base)
			if err != nil {
				return failProviderStage(stderr, "find pull request for escalation digest", err, "")
			}
			if found {
				reader, ok := provider.(pullRequestReader)
				if !ok {
					pf(stderr, "error: provider cannot read pull request for escalation digest\n")
					return 1
				}
				summary, err := reader.GetPullRequest(ctx, repo, strconv.Itoa(pr.Number))
				if err != nil {
					return failProviderStage(stderr, "read pull request for escalation digest", err, "")
				}
				body, err := withImplementationEscalationMarker(summary.Body, escalation)
				if err != nil {
					pf(stderr, "error: render duplicate-diff escalation: %v\n", err)
					return 1
				}
				if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
					Repository: repo,
					ID:         strconv.Itoa(pr.Number),
					Body:       &body,
				}); err != nil {
					return failProviderStage(stderr, "record escalated diff on pull request", err, "")
				}
			}
		}
		// #3564: embed the reviewer's actual verdict — rationale and every
		// finding's severity/message/location — plus the run id, so the
		// parked issue is self-contained instead of pointing a human at
		// run artifacts they'd have to parse by hand. Appended after the
		// question validation above so a needs-human park still states its
		// question, and the evidence follows it.
		verdict, gateName, found, err := issueCloseOutReviewVerdict(root, runID)
		if err != nil {
			pf(stderr, "error: resolve review verdict for escalation comment: %v\n", err)
			return 1
		}
		if found {
			comment += issueCloseOutVerdictDetail(verdict, gateName, runID)
		}
		// #2028: needs-remediation never gets the configured human assignee —
		// withNeedsHumanAssignee only fires for LabelNeedsHuman — so config
		// load is scoped to the status that actually needs it.
		parkLabel := providers.LabelNeedsHuman
		var assignee string
		if status == issueCloseOutNeedsHuman {
			selection, selectErr := stageJournalSelection()
			if selectErr != nil {
				pf(stderr, "error: select journal plane: %v\n", selectErr)
				return 1
			}
			if selection.OnPlane() {
				assignee = os.Getenv(executor.NeedsHumanAssigneeEnvVar)
			} else {
				cfg, configErr := instance.LoadConfig(l.ConfigFile())
				if configErr != nil {
					pf(stderr, "error: load needs-human routing config: %v\n", configErr)
					return 1
				}
				assignee = cfg.NeedsHumanAssignee
			}
		} else {
			parkLabel = needsRemediationLabel
		}
		req := withNeedsHumanAssignee(providers.UpdateWorkItemRequest{
			Repository:   backlogRepo,
			ID:           claim.ItemID,
			Comment:      comment,
			AddLabels:    []string{parkLabel},
			RemoveLabels: []string{providers.LabelReady},
		}, assignee)
		if _, err := provider.UpdateWorkItem(ctx, req); err != nil {
			pf(stderr, "error: park work item: %v\n", err)
			return 1
		}
	} else {
		head := providerInput("head", providers.BranchNameIn(providerBranchNamespace(), workflow, runID))
		base := providerInput("base", providerBaseBranch())
		pr, found, err := provider.FindPullRequestByBranch(ctx, repo, head, base)
		if err != nil {
			return failProviderStage(stderr, "find pull request", err, "")
		}
		if comment == "" {
			switch {
			case !found:
				comment = "Implementation complete."
			case status == providers.WorkItemStatusInReview:
				comment = implementationInReviewComment(pr.URL)
			default:
				comment = fmt.Sprintf("Implemented in %s.", pr.URL)
			}
		}
		if _, err := provider.UpdateWorkItemStatus(ctx, providers.UpdateWorkItemStatusRequest{
			Repository: backlogRepo,
			ID:         claim.ItemID,
			Status:     status,
			Comment:    comment,
		}); err != nil {
			pf(stderr, "error: update work item status: %v\n", err)
			return 1
		}
	}

	// Release the goobers:claimed label on the same event that releases the
	// ledger claim below (#414 design point 1), regardless of status — even
	// the in-review branch above releases the ledger claim unconditionally,
	// and UpdateWorkItemStatus only ever swaps goobers/status:-prefixed
	// labels, so without this the claim marker survived indefinitely and a
	// fresh eligibility query could see a completed (or in-review) item as
	// still "claimed" forever. Best-effort like the ClaimWorkItem marker on
	// the claim side (backlogquery.go): the durable ledger release below,
	// not this label, is what's actually authoritative for eligibility, so a
	// failed removal here leaves only a stale human-visible marker, not a
	// stuck item.
	// The claim marker is the plain LabelClaimed on every provider —
	// ClaimWorkItem defaults req.ClaimLabel to it on ADO too, so removal
	// uses the same constant everywhere. (A prior status-form translation
	// here targeted a tag the claim path never writes, wire-confirmed as
	// the stale-marker leak on ADO work items.)
	claimMarker := providers.LabelClaimed
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   backlogRepo,
		ID:           claim.ItemID,
		RemoveLabels: []string{claimMarker},
	}); err != nil {
		pf(stderr, "warning: release %s claim label: %v\n", claim.ItemID, err)
	}

	// Release the lease now rather than waiting for it to expire — the run
	// is finished with this item, and RecoverExpired's periodic sweep
	// (goobers up, #131) should not have to reclaim it later.
	err = ledger.Locked(claimContext(), claimLockOperationCloseOutRelease, func(tx claimsclient.Ledger) error {
		return tx.ReleaseScoped(claimContext(), claimsclient.KeyForEntry(claim), runID)
	})
	if err != nil {
		pf(stderr, "warning: release claim %s: %v\n", claim.ItemID, err)
	}

	switch status {
	case issueCloseOutNeedsHuman:
		pf(stdout, "parked %s needs-human\n", claim.ItemID)
	case issueCloseOutNeedsRemediation:
		pf(stdout, "parked %s needs-remediation\n", claim.ItemID)
	case providers.WorkItemStatusInReview:
		pf(stdout, "marked %s in-review (open PR, awaiting merge-review)\n", claim.ItemID)
	default:
		pf(stdout, "closed out %s\n", claim.ItemID)
	}
	return 0
}
