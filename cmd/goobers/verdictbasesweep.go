package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/goobers/goobers/providers"
)

// verdictBaseScan stores one fetched page plus a fair retry queue. A failing
// PR never prevents later pages from being visited. PageSize stays fixed until
// wraparound, so changing the invocation's budget cannot skip offsets.
type verdictBaseScan struct {
	Page      int   `json:"page"`
	PageSize  int   `json:"pageSize"`
	Pending   []int `json:"pending,omitempty"`
	Retry     []int `json:"retry,omitempty"`
	RetryTurn bool  `json:"retryTurn,omitempty"`
}

func reconcileNativeBaseVerdicts(ctx context.Context, root string, provider remediationProvider, repo providers.RepositoryRef, base string, limit int, stderr io.Writer) error {
	if base == "" {
		return nil
	}
	r := newVerdictBaseReconciler(root, repo, provider, stderr)
	if r == nil {
		return nil
	}
	return sweepNativeBaseVerdicts(ctx, root, repo, base, limit, provider, func(number int) error { return r.reconcile(ctx, number, stderr) })
}

func sweepNativeBaseVerdicts(ctx context.Context, root string, repo providers.RepositoryRef, base string, limit int, provider remediationProvider, reconcile func(int) error) error {
	if limit < 1 || limit > maxPostMergeReconcileBatch {
		return fmt.Errorf("invalid native verdict sweep budget %d", limit)
	}
	var actionErrors []error
	err := withPostMergeReconcileSession(ctx, root, func(session *postMergeReconcileSession) error {
		ledger, err := session.read()
		if err != nil {
			return err
		}
		key := postMergeOpenPRScanKey(repo, base)
		scan := ledger.VerdictBaseScans[key]
		save := func() error { ledger.VerdictBaseScans[key] = scan; return session.write(ledger) }
		retryBudget := min(len(scan.Retry), limit/2)
		if limit == 1 && scan.RetryTurn && len(scan.Retry) > 0 {
			retryBudget = 1
		}
		scan.RetryTurn = !scan.RetryTurn
		attempted := map[int]bool{}
		for range retryBudget {
			number := scan.Retry[0]
			scan.Retry = scan.Retry[1:]
			attempted[number] = true
			if err := reconcile(number); err != nil {
				scan.Retry = append(scan.Retry, number)
				actionErrors = append(actionErrors, fmt.Errorf("retry native verdict for PR #%d: %w", number, err))
			}
			if err := save(); err != nil {
				return err
			}
		}
		budget := limit - retryBudget
		if budget == 0 {
			return save()
		}
		if len(scan.Pending) == 0 {
			if err := loadVerdictBasePage(ctx, provider, repo, base, limit, &scan); err != nil {
				return err
			}
			// Persist the fetched work before side effects; crashes replay at most
			// an idempotent withdrawal, not an entire forgotten page.
			if err := save(); err != nil {
				return err
			}
		}
		for len(scan.Pending) > 0 && budget > 0 {
			number := scan.Pending[0]
			scan.Pending = scan.Pending[1:]
			if !attempted[number] {
				budget--
				attempted[number] = true
				// A PR found again on a fresh page is also removed from its retry slot.
				scan.Retry = slices.DeleteFunc(scan.Retry, func(n int) bool { return n == number })
				if err := reconcile(number); err != nil {
					scan.Retry = append(scan.Retry, number)
					actionErrors = append(actionErrors, fmt.Errorf("reconcile native verdict for PR #%d: %w", number, err))
				}
			}
			if err := save(); err != nil {
				return err
			}
		}
		return nil
	})
	return errors.Join(append(actionErrors, err)...)
}

func loadVerdictBasePage(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, base string, limit int, scan *verdictBaseScan) error {
	if scan.Page < 1 || scan.PageSize < 1 || scan.PageSize > maxPostMergeReconcileBatch {
		scan.Page, scan.PageSize = 1, limit
	}
	list := func() ([]providers.PullRequestSummary, error) {
		return provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{Repository: repo, Base: base, Page: scan.Page, Limit: scan.PageSize, SkipCheckState: true})
	}
	prs, err := list()
	if err != nil {
		return err
	}
	if len(prs) == 0 && scan.Page > 1 {
		scan.Page, scan.PageSize = 1, limit
		prs, err = list()
		if err != nil {
			return err
		}
	}
	scan.Page++
	for _, pr := range prs {
		if pr.Number > 0 {
			scan.Pending = append(scan.Pending, pr.Number)
		}
	}
	return nil
}
