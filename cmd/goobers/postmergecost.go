package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/intsplit"
	"github.com/goobers/goobers/providers"
)

const postMergeCostSummaryMarker = "<!-- goobers:pr-cost-summary v1 -->"

type postMergeCostReceipt struct {
	Attribution  providers.Attribution
	DirectIssues map[string]bool
}

type postMergeCostReport struct {
	Receipts       map[string]postMergeCostReceipt
	Total          providers.CostReceipt
	ByWorkflow     map[string]providers.CostReceipt
	IssueNanoAIU   map[string]int64
	SummaryPresent bool
	// ObservedRuns is every run with a trusted attribution marker on the PR
	// or its issues, whether or not that run published a cost. It is the
	// denominator of the "cost known for N of M runs" disclosure (#6353).
	ObservedRuns map[string]bool
}

type postMergeCostCommentReader interface {
	ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error)
}

type issueCommentCostProvider interface {
	postMergeCostCommentReader
	AuthenticatedLogin(context.Context) (string, error)
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

// collectPostMergeCostReport treats provider comments as a replicated receipt
// log. A run can write several cumulative snapshots as it progresses, so only
// the highest journal sequence for each run is counted. Issue observations are
// retained independently of the winning snapshot because an early issue
// comment can be the only durable evidence that a run belongs to that issue.
func collectPostMergeCostReport(
	ctx context.Context,
	prReader, issueReader postMergeCostCommentReader,
	prRepo, issueRepo providers.RepositoryRef,
	pullNumber string,
	issueIDs []string,
	trustedPRAuthor, trustedIssueAuthor string,
) (postMergeCostReport, error) {
	report := postMergeCostReport{
		Receipts:     map[string]postMergeCostReceipt{},
		ByWorkflow:   map[string]providers.CostReceipt{},
		IssueNanoAIU: map[string]int64{},
		ObservedRuns: map[string]bool{},
	}
	var errs []error

	prComments, err := prReader.ListComments(ctx, prRepo, pullNumber)
	if err != nil {
		errs = append(errs, fmt.Errorf("list pull request cost receipts: %w", err))
	} else {
		for _, comment := range prComments {
			if isTrustedCostComment(comment, trustedPRAuthor) && strings.Contains(comment.Body, postMergeCostSummaryMarker) {
				report.SummaryPresent = true
			}
			addCostReceiptObservation(report.Receipts, report.ObservedRuns, comment, "", trustedPRAuthor)
		}
	}

	for _, issueID := range issueIDs {
		comments, err := issueReader.ListComments(ctx, issueRepo, issueID)
		if err != nil {
			errs = append(errs, fmt.Errorf("list issue #%s cost receipts: %w", issueID, err))
			continue
		}
		for _, comment := range comments {
			addCostReceiptObservation(report.Receipts, report.ObservedRuns, comment, issueID, trustedIssueAuthor)
		}
	}

	for _, receipt := range report.Receipts {
		addCostReceipt(&report.Total, *receipt.Attribution.Cost)
		workflow := strings.TrimSpace(receipt.Attribution.Workflow)
		if workflow == "" {
			workflow = "unknown"
		}
		aggregate := report.ByWorkflow[workflow]
		addCostReceipt(&aggregate, *receipt.Attribution.Cost)
		report.ByWorkflow[workflow] = aggregate
	}
	report.IssueNanoAIU = allocateIssueNanoAIU(report.Receipts, issueIDs)
	return report, errors.Join(errs...)
}

func collectGitHubPostMergeCostReport(
	ctx context.Context,
	prProvider, issueProvider issueCommentCostProvider,
	repo providers.RepositoryRef,
	pullNumber string,
	issueIDs []string,
	stderr io.Writer,
) postMergeCostReport {
	prAuthor, err := prProvider.AuthenticatedLogin(ctx)
	if err != nil {
		pf(stderr, "warning: resolve cost receipt author: %v\n", err)
	}
	issueAuthor, err := issueProvider.AuthenticatedLogin(ctx)
	if err != nil {
		pf(stderr, "warning: resolve issue cost receipt author: %v\n", err)
	}
	if prAuthor == "" && issueAuthor == "" {
		return postMergeCostReport{}
	}
	report, _, reconcileErr := reconcileIssueCommentCostSummary(
		ctx, prProvider, issueProvider, repo, pullNumber, issueIDs, prAuthor, issueAuthor,
	)
	if reconcileErr != nil {
		pf(stderr, "warning: %v\n", reconcileErr)
	}
	return report
}

func reconcileIssueCommentCostSummary(
	ctx context.Context,
	prProvider, issueProvider issueCommentCostProvider,
	repo providers.RepositoryRef,
	pullNumber string,
	issueIDs []string,
	trustedPRAuthor, trustedIssueAuthor string,
) (postMergeCostReport, bool, error) {
	report, collectErr := collectPostMergeCostReport(
		ctx,
		prProvider,
		issueProvider,
		repo,
		repo,
		pullNumber,
		issueIDs,
		trustedPRAuthor,
		trustedIssueAuthor,
	)
	body := renderPostMergeCostSummary(report)
	if body == "" || report.SummaryPresent {
		return report, false, collectErr
	}
	_, updateErr := prProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         pullNumber,
		Comment:    body,
	})
	if updateErr != nil {
		updateErr = fmt.Errorf("post pull request cost summary: %w", updateErr)
	}
	return report, updateErr == nil, errors.Join(collectErr, updateErr)
}

// adoPRThreadCostReader lists a PR's thread comments with their authors
// attributed by identity GUID (ADO-N5), so the shared display-name trust check
// (isTrustedCostComment against self.DisplayName) only accepts receipts and
// summary markers this identity wrote — never another identity that shares
// its display name.
type adoPRThreadCostReader struct {
	provider adoPostMergePRComments
	self     providers.ADOIdentity
}

func (r adoPRThreadCostReader) ListComments(ctx context.Context, repo providers.RepositoryRef, pullNumber string) ([]providers.Comment, error) {
	comments, err := r.provider.ListPullRequestThreadComments(ctx, repo, pullNumber)
	if err != nil {
		return nil, err
	}
	return adoAttributeCommentsByID(comments, r.self), nil
}

// adoAttributingCommentReader lists work-item comments with their authors
// attributed by identity GUID, the work-item counterpart of
// adoPRThreadCostReader: a comment from another identity that shares the
// Goobers display name is never trusted as a cost receipt.
type adoAttributingCommentReader struct {
	provider postMergeCostCommentReader
	self     providers.ADOIdentity
}

func (r adoAttributingCommentReader) ListComments(ctx context.Context, repo providers.RepositoryRef, id string) ([]providers.Comment, error) {
	comments, err := r.provider.ListComments(ctx, repo, id)
	if err != nil {
		return nil, err
	}
	return adoAttributeCommentsByID(comments, r.self), nil
}

// adoWorkItemCostIdentity is the identity whose work-item receipts are
// trusted: the work-item provider's own when it can report one (it may run
// under a different credential than the PR provider), otherwise prSelf.
func adoWorkItemCostIdentity(ctx context.Context, issueProvider adoWorkItemCloser, prSelf providers.ADOIdentity) providers.ADOIdentity {
	reader, ok := issueProvider.(adoIdentityReader)
	if !ok {
		return prSelf
	}
	self, err := reader.AuthenticatedIdentity(ctx)
	if err != nil || (strings.TrimSpace(self.ID) == "" && strings.TrimSpace(self.DisplayName) == "") {
		return prSelf
	}
	return self
}

func collectADOPostMergeCostReport(
	ctx context.Context,
	issueProvider adoWorkItemCloser,
	prProvider adoPostMergePRComments,
	backlogRepo, repo providers.RepositoryRef,
	pullNumber string,
	issueIDs []string,
	stderr io.Writer,
) postMergeCostReport {
	self, err := prProvider.AuthenticatedIdentity(ctx)
	if err != nil {
		pf(stderr, "warning: resolve cost receipt author: %v\n", err)
		return postMergeCostReport{}
	}
	// Both sides are attributed by identity GUID in their readers (ADO-N5):
	// PR threads against the PR identity, work-item comments against the
	// identity of the provider that wrote them.
	issueSelf := adoWorkItemCostIdentity(ctx, issueProvider, self)
	report, collectErr := collectPostMergeCostReport(
		ctx,
		adoPRThreadCostReader{provider: prProvider, self: self},
		adoAttributingCommentReader{provider: issueProvider, self: issueSelf},
		repo,
		backlogRepo,
		pullNumber,
		issueIDs,
		self.DisplayName,
		issueSelf.DisplayName,
	)
	if collectErr != nil {
		pf(stderr, "warning: %v\n", collectErr)
	}
	if body := renderPostMergeCostSummary(report); body != "" && !report.SummaryPresent {
		if _, err := prProvider.PostPullRequestThreadComment(ctx, repo, pullNumber, body); err != nil {
			pf(stderr, "warning: post pull request cost summary: %v\n", err)
		}
	}
	return report
}

func addCostReceiptObservation(receipts map[string]postMergeCostReceipt, observed map[string]bool, comment providers.Comment, issueID, trustedAuthor string) {
	if !isTrustedCostComment(comment, trustedAuthor) {
		return
	}
	attribution, ok, err := providers.ParseAttribution(comment.Body)
	if err != nil || !ok || strings.TrimSpace(attribution.Run) == "" {
		return
	}
	observed[attribution.Run] = true
	if attribution.Cost == nil {
		return
	}

	current, exists := receipts[attribution.Run]
	if !exists {
		current.DirectIssues = map[string]bool{}
	}
	if issueID != "" {
		current.DirectIssues[issueID] = true
	}
	if !exists || attribution.Cost.JournalSequence >= current.Attribution.Cost.JournalSequence {
		current.Attribution = attribution
	}
	receipts[attribution.Run] = current
}

func isTrustedCostComment(comment providers.Comment, trustedAuthor string) bool {
	return trustedAuthor != "" && strings.EqualFold(strings.TrimSpace(comment.Author), strings.TrimSpace(trustedAuthor))
}

func addCostReceipt(dst *providers.CostReceipt, src providers.CostReceipt) {
	addInt64Measure(&dst.InputTokens, src.InputTokens)
	addInt64Measure(&dst.OutputTokens, src.OutputTokens)
	addInt64Measure(&dst.CacheReadTokens, src.CacheReadTokens)
	addInt64Measure(&dst.CacheWriteTokens, src.CacheWriteTokens)
	addInt64Measure(&dst.ReasoningTokens, src.ReasoningTokens)
	addFloatMeasure(&dst.CopilotPremiumRequests, src.CopilotPremiumRequests)
	addInt64Measure(&dst.NanoAIU, src.NanoAIU)
	addFloatMeasure(&dst.CostUSD, src.CostUSD)
	dst.VendorEstimated = dst.VendorEstimated || src.VendorEstimated
}

func addInt64Measure(dst **int64, src *int64) {
	if src == nil {
		return
	}
	if *dst == nil {
		v := int64(0)
		*dst = &v
	}
	**dst += *src
}

func addFloatMeasure(dst **float64, src *float64) {
	if src == nil {
		return
	}
	if *dst == nil {
		v := float64(0)
		*dst = &v
	}
	**dst += *src
}

// allocateIssueNanoAIU follows the telemetry rollup's existing semantics
// without depending on telemetry.db: directly associated runs are divided
// among their issues, then PR-only runs follow those direct-cost weights (or
// split evenly when no issue has a direct measured cost).
func allocateIssueNanoAIU(receipts map[string]postMergeCostReceipt, issueIDs []string) map[string]int64 {
	allocations := make(map[string]int64, len(issueIDs))
	issueSet := make(map[string]bool, len(issueIDs))
	for _, issueID := range issueIDs {
		issueSet[issueID] = true
		allocations[issueID] = 0
	}

	var prOnly int64
	for _, receipt := range receipts {
		if receipt.Attribution.Cost == nil || receipt.Attribution.Cost.NanoAIU == nil {
			continue
		}
		var direct []string
		for issueID := range receipt.DirectIssues {
			if issueSet[issueID] {
				direct = append(direct, issueID)
			}
		}
		sort.Strings(direct)
		if len(direct) == 0 {
			prOnly += *receipt.Attribution.Cost.NanoAIU
			continue
		}
		for issueID, value := range splitInt64Evenly(*receipt.Attribution.Cost.NanoAIU, direct) {
			allocations[issueID] += value
		}
	}

	if prOnly == 0 || len(issueIDs) == 0 {
		return allocations
	}
	weights := make(map[string]int64, len(issueIDs))
	var weightTotal int64
	for _, issueID := range issueIDs {
		weights[issueID] = allocations[issueID]
		weightTotal += allocations[issueID]
	}
	if weightTotal == 0 {
		for issueID, value := range splitInt64Evenly(prOnly, append([]string(nil), issueIDs...)) {
			allocations[issueID] += value
		}
		return allocations
	}
	for issueID, value := range splitInt64ByWeight(prOnly, issueIDs, weights) {
		allocations[issueID] += value
	}
	return allocations
}

func splitInt64Evenly(total int64, keys []string) map[string]int64 {
	sort.Strings(keys)
	out := make(map[string]int64, len(keys))
	if len(keys) == 0 {
		return out
	}
	base := total / int64(len(keys))
	remainder := total % int64(len(keys))
	for i, key := range keys {
		out[key] = base
		if int64(i) < remainder {
			out[key]++
		}
	}
	return out
}

func splitInt64ByWeight(total int64, keys []string, weights map[string]int64) map[string]int64 {
	return intsplit.LargestRemainder(total, keys, func(key string) int64 {
		return weights[key]
	})
}

func mergedPullRequestComment(pullNumber string, report postMergeCostReport, issueID string) string {
	return mergedPullRequestCommentAt("#"+pullNumber, report, issueID)
}

// mergedPullRequestCommentAt is mergedPullRequestComment with the pull
// request named by pullRef ("#<n>", or its URL when the item lives on another
// provider; see postMergePullRequestRef).
func mergedPullRequestCommentAt(pullRef string, report postMergeCostReport, issueID string) string {
	comment := fmt.Sprintf("Merged in pull request %s.", pullRef)
	if report.Total.NanoAIU == nil {
		return comment
	}
	comment += "\n\n**Total Goobers cost for this PR:** " + formatPostMergeCost(*report.Total.NanoAIU, report.Total.VendorEstimated)
	if issueID != "" {
		comment += "\n**Cost attributed to this issue:** " + formatPostMergeCost(report.IssueNanoAIU[issueID], report.Total.VendorEstimated)
	}
	return comment + postMergeCostDisclosures(report)
}

func renderPostMergeCostSummary(report postMergeCostReport) string {
	if report.Total.NanoAIU == nil {
		return ""
	}
	body := "Thanks for using Goobers. Your cost for this PR was **" + formatPostMergeCost(*report.Total.NanoAIU, report.Total.VendorEstimated) + "**."
	if len(report.ByWorkflow) > 0 {
		workflows := make([]string, 0, len(report.ByWorkflow))
		for workflow := range report.ByWorkflow {
			workflows = append(workflows, workflow)
		}
		sort.Strings(workflows)
		body += "\n\n**Workflow breakdown:**"
		for _, workflow := range workflows {
			receipt := report.ByWorkflow[workflow]
			if receipt.NanoAIU != nil {
				body += fmt.Sprintf("\n- `%s`: %s", workflow, formatPostMergeCost(*receipt.NanoAIU, receipt.VendorEstimated))
			}
		}
	}
	return body + postMergeCostDisclosures(report) + "\n\n" + postMergeCostSummaryMarker
}

// postMergeCostCoverage counts the observed runs whose cost is known: a run
// that left a trusted attribution marker but no measured AIC (no published
// receipt, or tokens without a cost) makes the total a lower bound.
func postMergeCostCoverage(report postMergeCostReport) (known, total int) {
	runs := make(map[string]bool, len(report.ObservedRuns)+len(report.Receipts))
	for run := range report.ObservedRuns {
		runs[run] = true
	}
	for run, receipt := range report.Receipts {
		runs[run] = true
		if receipt.Attribution.Cost != nil && receipt.Attribution.Cost.NanoAIU != nil {
			known++
		}
	}
	return known, len(runs)
}

// postMergeCostDisclosures renders the epic #4383 disclosures (#6353): partial
// run coverage, and the footnote for amounts that include a vendor-reported
// estimate. It returns "" when the total is complete and fully billed.
func postMergeCostDisclosures(report postMergeCostReport) string {
	var out string
	if known, total := postMergeCostCoverage(report); known < total {
		out += fmt.Sprintf("\n\nCost known for %d of %d runs, so this total is a lower bound.", known, total)
	}
	if report.Total.VendorEstimated {
		out += "\n\n\\* Includes Claude costs, which are vendor-reported estimates normalized to AIC for totals."
	}
	return out
}

// formatPostMergeCost is formatNanoAIU with the estimate footnote marker.
func formatPostMergeCost(nanoAIU int64, estimated bool) string {
	if estimated {
		return formatNanoAIU(nanoAIU) + "\\*"
	}
	return formatNanoAIU(nanoAIU)
}

func formatNanoAIU(nanoAIU int64) string {
	return strconv.FormatFloat(float64(nanoAIU)/1e9, 'f', 2, 64) + " AIC"
}
