package main

import (
	"encoding/json"
	"io"
	"os"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/prqueue"
	"github.com/goobers/goobers/providers"
)

// observePRQueueClaims records a pre-selection observation. It must not turn
// label disagreement into authority to release another instance's claims.
func observePRQueueClaims(root string, repo providers.RepositoryRef, prs []providers.PullRequestSummary, report *prqueue.Report) {
	report.RepositoryKey = repo.CanonicalKey()
	report.Gaggle = providerGaggle()
	report.RunID, report.Workflow, _ = providerRunContext()
	var listing claimsclient.Listing
	ledger, err := openStageClaimLedger(layoutFor(root))
	if err == nil {
		listing, err = pullRequestClaimListing(ledger, report.Gaggle, repo.Provider)
	}
	labels := make(map[int]bool, len(report.Items))
	// Bound the lookup map by the report, not by an arbitrarily large queue.
	for _, item := range report.Items {
		labels[item.Number] = false
	}
	for _, pr := range prs {
		if _, included := labels[pr.Number]; included {
			labels[pr.Number] = hasAnyLabel(pr.Labels, []string{providers.LabelClaimed})
		}
	}
	for i := range report.Items {
		item := &report.Items[i]
		entry, _, comparable := resolvePullRequestClaim(listing, report.Gaggle, repo.Provider, item.Number, report.ObservedAt)
		item.Claim = prqueue.ObserveClaim(err == nil, entry.RunID, report.RunID, entry.ExpiresAt, report.ObservedAt, labels[item.Number])
		if err == nil && !comparable {
			item.Claim.State = "held-in-legacy-namespace"
			item.Claim.NextStep = "Inspect the legacy unscoped lease; matching run IDs do not authorize a scoped claimant to adopt it."
		}
	}
}

func writePRQueueNoWork(stdout, stderr io.Writer, reason string, report *prqueue.Report) int {
	if report == nil {
		return writeNoWorkResult(stdout, stderr, reason)
	}
	// Preserve the existing no-work filename fallback as well as its scalar
	// outputs. The executor retains this entire JSON file as an artifact;
	// nested queueEligibility is deliberately not flattened into stage inputs.
	path := providerInput("resultFile", "claimed-item.json")
	data, err := json.Marshal(map[string]any{
		"claimed": false, "noWork": true, "noWorkReason": reason,
		"queueEligibility":        report,
		"queueEligibilityVersion": "1",
	})
	if err == nil {
		err = os.WriteFile(path, data, 0o644)
	}
	if err != nil {
		pf(stderr, "error: write queue no-work result: %v\n", err)
		return 1
	}
	pf(stdout, "no work: %s\n", reason)
	return 0
}
