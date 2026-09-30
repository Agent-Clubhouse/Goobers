package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/providers"
)

// readyHistoryProvider serves ListWorkItemLabelTransitionsForItem from a
// per-item table; every other provider method is unused by the ready-age
// annotation and panics through the nil embedded interface if reached.
type readyHistoryProvider struct {
	backlogIssueProvider
	transitions map[string][]providers.WorkItemLabelTransition
	errs        map[string]error
}

func (p readyHistoryProvider) ListWorkItemLabelTransitionsForItem(_ context.Context, _ providers.RepositoryRef, id, _ string) ([]providers.WorkItemLabelTransition, error) {
	if err := p.errs[id]; err != nil {
		return nil, err
	}
	return p.transitions[id], nil
}

func newReadyHistorySession(t *testing.T, provider readyHistoryProvider, stderr *bytes.Buffer, ids ...string) (*backlogClaimSession, *claimsclient.File) {
	t.Helper()
	ledger, err := claimsclient.NewFile(claimsclient.FileConfig{LedgerPath: filepath.Join(t.TempDir(), "claims.json")})
	if err != nil {
		t.Fatal(err)
	}
	session := &backlogClaimSession{
		env: backlogQueryEnv{
			repo:          providers.RepositoryRef{Provider: providers.ProviderADO, Project: "example-project"},
			issueProvider: provider,
			stderr:        stderr,
		},
		ledger: ledger, runID: "run-1", leaseDuration: time.Hour,
	}
	for _, id := range ids {
		if ok, _, err := ledger.ClaimScoped(t.Context(), claimsclient.Key{ExternalID: id}, "run-1", "claim", time.Hour); err != nil || !ok {
			t.Fatalf("seed claim %s: ok=%v err=%v", id, ok, err)
		}
		item := providers.WorkItem{ID: id, State: "open", Labels: []string{providers.LabelReady}}
		session.claimed = append(session.claimed, item)
		session.newlyClaimed = append(session.newlyClaimed, item)
	}
	return session, ledger
}

// TestAnnotateNewReadyClaimsSkipsHistoryIncompleteItem pins that an item whose
// label history the provider reports incomplete (an ADO item past the
// revision cap, or a tag change with no change date) is released and skipped
// like a malformed item, and the claim stage goes on to the next item instead
// of failing every time the item is re-selected.
func TestAnnotateNewReadyClaimsSkipsHistoryIncompleteItem(t *testing.T) {
	readyAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	provider := readyHistoryProvider{
		errs: map[string]error{
			"7": fmt.Errorf("%w: ADO work item 7 has reached the 10000-revision cap", providers.ErrLabelHistoryIncomplete),
		},
		transitions: map[string][]providers.WorkItemLabelTransition{
			"8": {{EventID: 1, ItemID: "8", Label: providers.LabelReady, Added: true, OccurredAt: readyAt}},
		},
	}
	var stderr bytes.Buffer
	session, ledger := newReadyHistorySession(t, provider, &stderr, "7", "8")

	skipped, code := session.annotateNewReadyClaims(t.Context(), 0)
	if code != 0 || skipped != 1 {
		t.Fatalf("annotateNewReadyClaims = (%d, %d), stderr = %q; want one skipped item and exit 0", skipped, code, stderr.String())
	}
	if len(session.claimed) != 1 || session.claimed[0].ID != "8" {
		t.Fatalf("claimed = %+v, want only item 8", session.claimed)
	}
	if len(session.newlyClaimed) != 1 || session.newlyClaimed[0].ID != "8" {
		t.Fatalf("newlyClaimed = %+v, want item 7 forgotten", session.newlyClaimed)
	}
	if !strings.Contains(stderr.String(), "skipping malformed eligible item 7") ||
		!strings.Contains(stderr.String(), "revision cap") {
		t.Fatalf("stderr = %q, want a skip warning naming item 7 and its cause", stderr.String())
	}
	entries, err := ledger.ForRunAll(t.Context(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ExternalID != "8" {
		t.Fatalf("ledger entries = %+v, want item 7 released and item 8 held", entries)
	}
}

// TestAnnotateNewReadyClaimsFailsOnOtherProviderErrors pins that only the
// per-item incomplete-history error is skipped: a transport or auth failure
// still fails the stage.
func TestAnnotateNewReadyClaimsFailsOnOtherProviderErrors(t *testing.T) {
	provider := readyHistoryProvider{errs: map[string]error{"7": errors.New("ado: service unavailable")}}
	var stderr bytes.Buffer
	session, _ := newReadyHistorySession(t, provider, &stderr, "7")
	t.Chdir(t.TempDir()) // the stage failure writes claimed-item.json

	if _, code := session.annotateNewReadyClaims(t.Context(), 0); code == 0 {
		t.Fatalf("annotateNewReadyClaims exit = 0, stderr = %q; want the provider failure to fail the stage", stderr.String())
	}
	if len(session.claimed) != 1 {
		t.Fatalf("claimed = %+v, want the item kept for the stage's own rollback", session.claimed)
	}
}
