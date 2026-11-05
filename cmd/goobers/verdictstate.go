package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/findingledger"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// verdictstate.go is the verdict-json half of Goobers#3025/#5030: the
// merge-review verdict that drives pr-remediation's routing/budgets, moved
// off the `<!-- verdict-json: ... -->` PR-comment payload onto the same
// closed scheduler-state KV plane failurestreakstate.go uses for the
// failure-streak half.
//
// The PR comment stays (renderVerdictComment): it is still the only
// cross-repository-provider-visible record a human or an Azure DevOps PR
// thread reader sees, and Gitea/GitHub tooling that renders it. It is written
// AFTER the authoritative key update succeeds, so it is a projection —
// editing or deleting it can no longer change what gatherPRVerdict returns
// once the key exists.
//
// gatherPRVerdict, this file's reader, falls back to legacy
// `<!-- verdict-json: ... -->` comments when the key is absent. It folds all
// trusted marked records before opportunistically migrating the result into
// the key, so migration retains findings from non-canonical comments too.
const (
	verdictStateSchema        = "goobers.dev/remediation-verdict/v1"
	verdictStateLockOperation = "remediation-verdict.update"
)

// verdictRecordKey is the record's logical identity: the repository's
// provider-complete canonical identity plus the PR number, so identically
// numbered PRs in different repositories or providers never collide.
func verdictRecordKey(repo providers.RepositoryRef, prNumber int) string {
	return repo.CanonicalKey() + "#" + strconv.Itoa(prNumber)
}

// verdictStateKey is the scheduler-state key holding one PR's verdict
// record.
func verdictStateKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return stateclient.RemediationVerdictKey(fmt.Sprintf("%x", sum))
}

var verdictRecordSpec = keyedStateRecordSpec[apiv1.Verdict]{
	schema:      verdictStateSchema,
	operation:   verdictStateLockOperation,
	errorPrefix: "decode remediation-verdict state",
	field:       keyedStateRecordFieldVerdict,
	stateKey:    verdictStateKey,
}

func decodeVerdictRecord(value stateclient.Value, key string) (apiv1.Verdict, bool, error) {
	return decodeKeyedStateRecord(value, key, verdictRecordSpec)
}

// updateVerdictRecord is the record's read-modify-write: one lock acquisition
// on the file backend, one compare-and-swap on the plane. fn returns
// write=false to leave the key untouched, and MUST be safe to run more than
// once — the plane re-runs it against the new value when a CAS loses.
func updateVerdictRecord(
	ctx context.Context,
	store stateclient.Store,
	key string,
	fn func(apiv1.Verdict, bool) (apiv1.Verdict, bool, error),
) error {
	return updateKeyedStateRecord(ctx, store, key, verdictRecordSpec, fn)
}

// loadVerdictState reads a PR's authoritative verdict record. It answers
// ok=false when the key is absent — that is the caller's cue to fall back to
// parsing the legacy comment marker and migrate on read, exactly as
// loadFailureStreakState does for the failure-streak half.
func loadVerdictState(root string, repo providers.RepositoryRef, prNumber int) (apiv1.Verdict, bool, error) {
	store, err := openStageStateStore(instance.NewLayout(root))
	if err != nil {
		return apiv1.Verdict{}, false, fmt.Errorf("open remediation-verdict state: %w", err)
	}
	key := verdictRecordKey(repo, prNumber)
	value, err := store.Get(stateContext(), verdictStateKey(key))
	if err != nil {
		return apiv1.Verdict{}, false, fmt.Errorf("read remediation-verdict state for PR #%d: %w", prNumber, err)
	}
	return decodeVerdictRecord(value, key)
}

// migrateVerdictState writes verdict into the key only if it is still
// absent — a concurrent writer's value wins — used by gatherPRVerdict's
// migration-on-read path. Best-effort: a failure to migrate is not fatal to
// the caller, which already has the legacy-parsed verdict to fall back on.
func migrateVerdictState(root string, repo providers.RepositoryRef, prNumber int, verdict apiv1.Verdict) {
	store, err := openStageStateStore(instance.NewLayout(root))
	if err != nil {
		return
	}
	key := verdictRecordKey(repo, prNumber)
	_ = withVerdictPublicationCustody(stateContext(), root, repo, func(ctx context.Context) error {
		return updateVerdictRecord(ctx, store, key, func(current apiv1.Verdict, exists bool) (apiv1.Verdict, bool, error) {
			if exists {
				return current, false, nil
			}
			return verdict, true, nil
		})
	})
}

// verdictRecordsForPR returns a pull request's published merge-review verdict
// records in publication order (#5890), for findingledger to replay:
//
//   - The trusted author's marked status comments only, never other
//     comments that happen to carry a verdict-json payload (a remediation
//     response quoting agent text, say). On GitHub and Gitea the canonical
//     status comment is edited in place, so the oldest marked comment holds
//     the newest record: it is read after any other marked comment. Azure
//     DevOps threads are append-only, so their order is publication order.
//   - The authoritative scheduler-state record last: it is written before
//     each comment, so it is never older than one.
//
// A failure to read the scheduler-state record is returned, not skipped:
// the ledger must not silently lose its newest record.
func verdictRecordsForPR(root string, repo providers.RepositoryRef, prNumber int, comments []providers.Comment, author string) ([]apiv1.Verdict, error) {
	var records []apiv1.Verdict
	var canonical *apiv1.Verdict
	for _, comment := range mergeReviewStatusComments(comments, author) {
		verdict, ok := parseVerdictComment(comment.Body)
		switch {
		case !ok:
		case canonical == nil && repo.Provider != providers.ProviderADO:
			canonical = &verdict
		default:
			records = append(records, verdict)
		}
	}
	if canonical != nil {
		records = append(records, *canonical)
	}
	if root == "" {
		return records, nil
	}
	verdict, ok, err := loadVerdictState(root, repo, prNumber)
	if err != nil {
		return nil, err
	}
	if ok {
		records = append(records, verdict)
	}
	return records, nil
}

// prFindingPolicy is the liberal substantive-finding test the ledger applies
// to a pull request's records: any code-change or unclassed finding about this
// pull request, whatever its severity. A caller's own severity floor applies
// afterwards, to the findings this keeps.
func prFindingPolicy(prNumber int) findingledger.Substantive {
	target := strconv.Itoa(prNumber)
	return func(finding apiv1.Finding) bool {
		return substantiveFindingAppliesToPR(finding, target, apiv1.SeverityInfo)
	}
}

// carryOutstandingFindings folds the ledger of records into latest (#5890):
// every finding an earlier review raised on latest's head that no review has
// resolved or rejected is written to its StandingFindings, and on a pass or
// needs-changes also to its Findings, a pass becoming needs-changes. A read
// (forRead) lists the standing findings in Findings whatever the decision, so
// every healer and the remediation brief see them. standing names them.
func carryOutstandingFindings(records []apiv1.Verdict, latest apiv1.Verdict, prNumber int, forRead bool) (apiv1.Verdict, []string) {
	if snapshot, ok := findingledger.LatestSnapshot(records, latest.HeadSHA); ok && snapshot.LedgerGeneration >= latest.LedgerGeneration {
		latest = snapshot
	}
	folded, result := findingledger.Fold(records, latest, prFindingPolicy(prNumber))
	if len(result.Standing) == 0 {
		return folded, nil
	}
	if forRead {
		folded.Findings = findingledger.AppendMissing(folded.Findings, folded.StandingFindings, prFindingPolicy(prNumber))
	}
	note := fmt.Sprintf("Standing from an earlier review of this head, never resolved or rejected: %s.", strings.Join(result.Standing, ", "))
	if folded.Rationale == "" {
		folded.Rationale = note
	} else if !strings.Contains(folded.Rationale, note) {
		folded.Rationale += "\n\n" + note
	}
	return folded, result.Standing
}

// publishADOReviewedVerdict publishes a merge-review verdict on Azure DevOps
// after folding the earlier verdict records on the PR's threads into it
// (#5890). A pass that left a finding on its head standing is published
// through the non-pass path, so it never reaches merge-pr.
//
// Unreadable identity or threads fail publication. Before the first committed
// generation, scheduler state alone may omit legacy findings. Publishing that
// incomplete snapshot would permanently supersede the unread legacy evidence.
func publishADOReviewedVerdict(ctx context.Context, root string, provider *providers.ADOProvider, repo providers.RepositoryRef, number int, current providers.PullRequestSummary, verdict apiv1.Verdict, publication verdictPublication, resultFile string, stdout, stderr io.Writer) int {
	var comments []providers.Comment
	self, err := provider.AuthenticatedIdentity(ctx)
	if err == nil {
		comments, err = provider.ListPullRequestThreadComments(ctx, repo, strconv.Itoa(number))
	}
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("read earlier verdicts on PR #%d", number), err, resultFile)
	}
	verdict.HeadSHA, verdict.BaseSHA = current.HeadSHA, current.BaseSHA
	legacy, err := verdictRecordsForPR("", repo, number, adoAttributeCommentsByID(comments, self), self.DisplayName)
	if err != nil {
		return failProviderStage(stderr, "read legacy verdicts", err, resultFile)
	}
	folded, err := publication.commit(verdict, legacy, nil, stdout)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("fold earlier verdicts for PR #%d", number), err, resultFile)
	}
	if folded.Decision != apiv1.VerdictPass {
		return publishADONonPassVerdict(ctx, root, provider, repo, number, current, folded, resultFile, stdout, stderr)
	}
	return publishADOPassVerdict(ctx, root, provider, repo, number, current, folded, resultFile, stdout, stderr)
}

// outstandingFindingMergeRefusals is merge-pr's #5890 conjunct, checked under
// the merge lock against the locked poll: no finding an earlier review raised
// on the live head may stand unanswered, whatever later pass authorized this
// merge. It returns the refusal reason, or none when nothing stands. Every
// failure to read the records refuses too, as does a verdict author that
// cannot be named: a merge must not land on evidence it could not read. Every
// refusal is journaled.
func outstandingFindingMergeRefusals(ctx context.Context, root string, provider any, repo providers.RepositoryRef, pullNumber, verdictAuthor string, poll providers.PullRequestPollResult) []string {
	number, err := strconv.Atoi(pullNumber)
	if err != nil {
		return []string{fmt.Sprintf("pull number %q is not a number, so earlier review findings cannot be checked (#5890)", pullNumber)}
	}
	comments, author, err := mergeVerdictComments(ctx, provider, repo, pullNumber, verdictAuthor, poll)
	var records []apiv1.Verdict
	if err == nil {
		records, err = verdictRecordsForPR(root, repo, number, comments, author)
	}
	if err != nil {
		return []string{fmt.Sprintf("could not read the earlier merge-review verdicts (%v), so no finding can be shown resolved (#5890)", err)}
	}
	outstanding := findingledger.Outstanding(records, poll.HeadSHA, prFindingPolicy(number))
	if len(outstanding) == 0 {
		return nil
	}
	ids := make([]string, 0, len(outstanding))
	for _, finding := range outstanding {
		ids = append(ids, finding.ID)
	}
	recordMergeSafetyBlock(root, "merge-pr", "refuse-merge", number, poll.HeadSHA, poll.BaseSHA, ids)
	return []string{fmt.Sprintf("%d finding(s) an earlier review raised on this head were never resolved or rejected (%s), and a later pass does not clear them (#5890)",
		len(ids), strings.Join(ids, ", "))}
}

// mergeVerdictComments returns the comments merge-pr reads verdict records
// from and the author it trusts for them. GitHub and Gitea comments come from
// the locked poll, trusted for verdictAuthor or, without one, the merge
// identity. Azure DevOps polls carry none, so its PR threads are read here,
// attributed to this identity by GUID (ADO-N5).
func mergeVerdictComments(ctx context.Context, provider any, repo providers.RepositoryRef, pullNumber, verdictAuthor string, poll providers.PullRequestPollResult) ([]providers.Comment, string, error) {
	if reader, ok := provider.(adoThreadVerdictReader); ok && repo.Provider == providers.ProviderADO {
		self, err := reader.AuthenticatedIdentity(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("resolve merge-review verdict author: %w", err)
		}
		threads, err := reader.ListPullRequestThreadComments(ctx, repo, pullNumber)
		if err != nil {
			return nil, "", fmt.Errorf("list thread comments: %w", err)
		}
		return adoAttributeCommentsByID(threads, self), self.DisplayName, nil
	}
	author := strings.TrimSpace(verdictAuthor)
	if author == "" {
		identity, ok := provider.(interface {
			AuthenticatedLogin(context.Context) (string, error)
		})
		if !ok {
			return nil, "", fmt.Errorf("no verdictAuthor input and the merge identity cannot be named")
		}
		login, err := identity.AuthenticatedLogin(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("resolve merge-review verdict author: %w", err)
		}
		author = login
	}
	comments := make([]providers.Comment, 0, len(poll.CommentsSince))
	for _, comment := range poll.CommentsSince {
		comments = append(comments, providers.Comment{Author: comment.Author, Body: comment.Body})
	}
	return comments, author, nil
}

// renderFindingDispositions renders a verdict's answers to earlier findings,
// and the findings still standing, for the human reading the status comment
// (#5890).
func renderFindingDispositions(v apiv1.Verdict) string {
	s := ""
	if len(v.StandingFindings) > 0 {
		s += "\n\nStanding from an earlier review of this head (resolve or reject each to clear it):"
		for _, f := range v.StandingFindings {
			s += fmt.Sprintf("\n- `%s` [%s] %s", f.ID, f.Severity, f.Message)
		}
	}
	if len(v.Dispositions) > 0 {
		s += "\n\nEarlier findings answered:"
		for _, d := range v.Dispositions {
			detail := d.Reason
			if d.Outcome == apiv1.FindingResolved {
				detail = d.Evidence
			}
			s += fmt.Sprintf("\n- %s `%s`: %s", d.Outcome, d.FindingID, strings.TrimSpace(detail))
		}
	}
	return s
}

// recordMergeSafetyBlock journals a merge-safety enforcement (#5890/#6021)
// so every block is observable: which stage refused what, on which pull
// request state, because of which findings. Best-effort, like every stage
// annotation: the refusal itself never depends on the journal write.
func recordMergeSafetyBlock(root, stage, action string, prNumber int, head, base string, findingIDs []string) {
	if root == "" {
		return
	}
	annotations, err := openStageAnnotator(instance.NewLayout(root))
	if err != nil {
		return
	}
	_ = annotations.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"annotation": "merge-safety-block",
			"check":      "undispositioned-finding",
			"stage":      stage,
			"action":     action,
			"pullNumber": prNumber,
			"headSha":    head,
			"baseSha":    base,
			"findingIds": findingIDs,
		},
	})
	_ = annotations.Close()
}
