//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationDesignDeliveryLedgersMatchIssueState is #4518's forge-aware
// reconciliation: the half of "is this status true?" that no offline check can
// answer.
//
// The merge-tier `designstatus` check proves a document's metadata is
// internally coherent — an `implemented` page names delivery, a `superseded`
// page names its successor, a supersession is reciprocal. It cannot know
// whether the issues that delivery ledger names are actually closed. That gap
// is exactly where the 2026-09-06 audit found the corpus rotting in both
// directions:
//
//   - a design marked `implemented` whose delivery issues are still open —
//     the status ran ahead of the work; and
//   - a design still marked `draft` or `approved` whose entire delivery ledger
//     has closed — the work ran ahead of the status, which is the more common
//     and more damaging case, because it invites someone to redesign a shipped
//     feature.
//
// It is deliberately REPORTING, not merge-blocking: issue state is external and
// moves without any change to this repository, so it belongs on a schedule
// beside the tracked-gap check rather than in the merge gate. It runs from the
// repository root the way the command does.
func TestIntegrationDesignDeliveryLedgersMatchIssueState(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_GITHUB_TOKEN", "GITHUB_TOKEN")

	token := os.Getenv("GOOBERS_GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	spec := os.Getenv("GOOBERS_DESIGN_LEDGER_REPO")
	if spec == "" {
		spec = "Agent-Clubhouse/Goobers"
	}
	owner, name, ok := strings.Cut(spec, "/")
	if !ok || owner == "" || name == "" {
		t.Fatalf("GOOBERS_DESIGN_LEDGER_REPO = %q, want owner/name", spec)
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	docs, problems := loadDocuments(designRoot, adrRoot)
	for _, problem := range problems {
		t.Error(problem)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: owner, Name: name}
	provider := providers.NewGitHubProvider(token)

	// One lookup per distinct issue: delivery ledgers overlap heavily across a
	// design family, and the forge's rate limit is the binding constraint.
	closed := map[string]bool{}
	resolve := func(ref string) (bool, bool) {
		if state, seen := closed[ref]; seen {
			return state, true
		}
		item, err := provider.GetWorkItem(ctx, repo, strings.TrimPrefix(ref, "#"))
		if err != nil {
			t.Errorf("resolve %s on %s: %v", ref, spec, err)
			return false, false
		}
		state, err := deliveryReferenceClosed(item, func(id string) (providers.PullRequestSummary, error) {
			return provider.GetPullRequest(ctx, repo, id)
		})
		if err != nil {
			t.Errorf("resolve delivery %s on %s: %v", ref, spec, err)
			return false, false
		}
		closed[ref] = state
		return state, true
	}

	var checked int
	for _, doc := range docs {
		if len(doc.DeliveredBy)+len(doc.Tracking)+len(doc.Remaining) == 0 {
			continue
		}
		checked++
		for _, problem := range reconcileDelivery(doc, resolve) {
			t.Error(problem)
		}
	}
	t.Logf("reconciled %d design document(s) with a delivery ledger against %s", checked, spec)
}
