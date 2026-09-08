package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

const postMergeCostSummaryMarker = "<!-- goobers:pr-cost-summary v1 -->"

type postMergeCostReceipt struct {
	Attribution  providers.Attribution
	DirectIssues map[string]bool
}

type postMergeCostReport struct {
	Receipts     map[string]postMergeCostReceipt
	Total        providers.CostReceipt
	ByWorkflow   map[string]providers.CostReceipt
	IssueNanoAIU map[string]int64
	SummaryBody  string
}

type postMergeCostCommentReader interface {
	ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error)
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
	}
	var errs []error

	prComments, err := prReader.ListComments(ctx, prRepo, pullNumber)
	if err != nil {
		errs = append(errs, fmt.Errorf("list pull request cost receipts: %w", err))
	} else {
		for _, comment := range prComments {
			if isTrustedCostComment(comment, trustedPRAuthor) && strings.Contains(comment.Body, postMergeCostSummaryMarker) {
				report.SummaryBody = comment.Body
			}
			addCostReceiptObservation(report.Receipts, comment, "", trustedPRAuthor)
		}
	}

	for _, issueID := range issueIDs {
		comments, err := issueReader.ListComments(ctx, issueRepo, issueID)
		if err != nil {
			errs = append(errs, fmt.Errorf("list issue #%s cost receipts: %w", issueID, err))
			continue
		}
		for _, comment := range comments {
			addCostReceiptObservation(report.Receipts, comment, issueID, trustedIssueAuthor)
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
	prProvider, issueProvider remediationProvider,
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
	report, collectErr := collectPostMergeCostReport(ctx, prProvider, issueProvider, repo, repo, pullNumber, issueIDs, prAuthor, issueAuthor)
	if collectErr != nil {
		pf(stderr, "warning: %v\n", collectErr)
	}
	if body := renderPostMergeCostSummary(report); body != "" && body != report.SummaryBody {
		if _, err := prProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo,
			ID:         pullNumber,
			Comment:    body,
		}); err != nil {
			pf(stderr, "warning: post pull request cost summary: %v\n", err)
		}
	}
	return report
}

type adoPRThreadCostReader struct {
	provider adoPostMergePRComments
}

func (r adoPRThreadCostReader) ListComments(ctx context.Context, repo providers.RepositoryRef, pullNumber string) ([]providers.Comment, error) {
	return r.provider.ListPullRequestThreadComments(ctx, repo, pullNumber)
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
	author, err := prProvider.AuthenticatedLogin(ctx)
	if err != nil {
		pf(stderr, "warning: resolve cost receipt author: %v\n", err)
		return postMergeCostReport{}
	}
	report, collectErr := collectPostMergeCostReport(
		ctx,
		adoPRThreadCostReader{provider: prProvider},
		issueProvider,
		repo,
		backlogRepo,
		pullNumber,
		issueIDs,
		author,
		author,
	)
	if collectErr != nil {
		pf(stderr, "warning: %v\n", collectErr)
	}
	if body := renderPostMergeCostSummary(report); body != "" && body != report.SummaryBody {
		if _, err := prProvider.PostPullRequestThreadComment(ctx, repo, pullNumber, body); err != nil {
			pf(stderr, "warning: post pull request cost summary: %v\n", err)
		}
	}
	return report
}

func addCostReceiptObservation(receipts map[string]postMergeCostReceipt, comment providers.Comment, issueID, trustedAuthor string) {
	if !isTrustedCostComment(comment, trustedAuthor) {
		return
	}
	attribution, ok, err := providers.ParseAttribution(comment.Body)
	if err != nil || !ok || attribution.Cost == nil || strings.TrimSpace(attribution.Run) == "" {
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
	for issueID, value := range splitInt64ByWeight(prOnly, issueIDs, weights, weightTotal) {
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

func splitInt64ByWeight(total int64, keys []string, weights map[string]int64, weightTotal int64) map[string]int64 {
	sort.Strings(keys)
	out := make(map[string]int64, len(keys))
	type remainder struct {
		key       string
		remainder *big.Int
	}
	remainders := make([]remainder, 0, len(keys))
	var assigned int64
	divisor := big.NewInt(weightTotal)
	for _, key := range keys {
		product := new(big.Int).Mul(big.NewInt(total), big.NewInt(weights[key]))
		quotient, rem := new(big.Int), new(big.Int)
		quotient.QuoRem(product, divisor, rem)
		out[key] = quotient.Int64()
		assigned += out[key]
		remainders = append(remainders, remainder{key: key, remainder: rem})
	}
	sort.SliceStable(remainders, func(i, j int) bool {
		if cmp := remainders[i].remainder.Cmp(remainders[j].remainder); cmp != 0 {
			return cmp > 0
		} else {
			return remainders[i].key < remainders[j].key
		}
	})
	for i := int64(0); i < total-assigned; i++ {
		out[remainders[i%int64(len(remainders))].key]++
	}
	return out
}

func mergedPullRequestComment(pullNumber string, report postMergeCostReport, issueID string) string {
	comment := fmt.Sprintf("Merged in pull request #%s.", pullNumber)
	if report.Total.NanoAIU == nil {
		return comment
	}
	comment += "\n\n**Total Goobers cost for this PR:** " + formatNanoAIU(*report.Total.NanoAIU)
	if issueID != "" {
		comment += "\n**Cost attributed to this issue:** " + formatNanoAIU(report.IssueNanoAIU[issueID])
	}
	return comment
}

func renderPostMergeCostSummary(report postMergeCostReport) string {
	if report.Total.NanoAIU == nil {
		return ""
	}
	body := "Thanks for using Goobers. Your cost for this PR was **" + formatNanoAIU(*report.Total.NanoAIU) + "**."
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
				body += fmt.Sprintf("\n- `%s`: %s", workflow, formatNanoAIU(*receipt.NanoAIU))
			}
		}
	}
	return body + "\n\n" + postMergeCostSummaryMarker
}

func formatNanoAIU(nanoAIU int64) string {
	return strconv.FormatFloat(float64(nanoAIU)/1e9, 'f', 2, 64) + " AIC"
}
