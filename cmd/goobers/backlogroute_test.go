package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// These tests cover personal-gaggle-routing §5.5's route transaction with a
// fake provider, exercising the four behaviours the ordering exists to
// guarantee: success, provider failure (which must RETAIN the authoritative
// lease), idempotent retry, and per-item outcomes within one batch.

// fakeRouteProvider stubs the narrow backlogIssueProvider slice routeOne uses,
// via the same embedded-nil-interface pattern fakeDependencyCheckProvider uses:
// only the three methods the transaction actually calls are implemented, so a
// call to anything else would panic loudly rather than silently pass.
type fakeRouteProvider struct {
	providers.Provider
	labels map[string][]string
	// updateErr/releaseErr fail the corresponding step for a specific item ID.
	updateErr  map[string]error
	releaseErr map[string]error
	getErr     map[string]error

	updateCalls  []string
	releaseCalls []string
}

func newFakeRouteProvider(labels map[string][]string) *fakeRouteProvider {
	return &fakeRouteProvider{
		labels:     labels,
		updateErr:  map[string]error{},
		releaseErr: map[string]error{},
		getErr:     map[string]error{},
	}
}

func (f *fakeRouteProvider) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	if err := f.getErr[id]; err != nil {
		return providers.WorkItem{}, err
	}
	return providers.WorkItem{ID: id, Labels: append([]string(nil), f.labels[id]...)}, nil
}

func (f *fakeRouteProvider) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.updateCalls = append(f.updateCalls, req.ID)
	if err := f.updateErr[req.ID]; err != nil {
		return providers.WorkItem{}, err
	}
	f.labels[req.ID] = append(f.labels[req.ID], req.AddLabels...)
	return providers.WorkItem{ID: req.ID, Labels: append([]string(nil), f.labels[req.ID]...)}, nil
}

func (f *fakeRouteProvider) ReleaseWorkItemClaim(_ context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	f.releaseCalls = append(f.releaseCalls, req.ID)
	if err := f.releaseErr[req.ID]; err != nil {
		return providers.WorkItem{}, err
	}
	remaining := f.labels[req.ID][:0]
	for _, label := range f.labels[req.ID] {
		if label != providers.LabelClaimed {
			remaining = append(remaining, label)
		}
	}
	f.labels[req.ID] = remaining
	return providers.WorkItem{ID: req.ID, Labels: append([]string(nil), remaining...)}, nil
}

func (f *fakeRouteProvider) ListWorkItemLabelTransitionsForItem(context.Context, providers.RepositoryRef, string, string) ([]providers.WorkItemLabelTransition, error) {
	return nil, nil
}

// routeFixture wires a real claim ledger and lock under a temp instance root,
// so ownership behaviour is exercised against the actual ledger rather than a
// stub of it.
type routeFixture struct {
	tx     routeTransaction
	ledger func(t *testing.T) *localscheduler.ClaimLedger
	key    func(itemID string) localscheduler.ClaimKey
}

func newRouteFixture(t *testing.T, provider backlogIssueProvider) routeFixture {
	t.Helper()
	root := t.TempDir()
	layout := instance.NewLayout(root)
	schedulerDir := layout.SchedulerDir()
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatalf("create scheduler dir: %v", err)
	}
	identity, err := apiv1.BacklogIdentityFromRef(apiv1.BacklogRef{
		Provider: apiv1.ProviderGitHub,
		Project:  "gim-home/brandiv.goobers",
	})
	if err != nil {
		t.Fatalf("backlog identity: %v", err)
	}
	ledgerPath := filepath.Join(schedulerDir, claimLedgerFileName)
	return routeFixture{
		tx: routeTransaction{
			layout:          layout,
			lockPath:        filepath.Join(schedulerDir, claimLockFileName),
			ledgerPath:      ledgerPath,
			runID:           "run-router",
			workflow:        "routing",
			gaggle:          "router",
			backlogIdentity: identity,
			backlogRepo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "gim-home", Name: "brandiv.goobers"},
			provider:        provider,
		},
		ledger: func(t *testing.T) *localscheduler.ClaimLedger {
			t.Helper()
			ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
			if err != nil {
				t.Fatalf("open ledger: %v", err)
			}
			return ledger
		},
		key: func(itemID string) localscheduler.ClaimKey {
			return backlogClaimKey(identity, "router", itemID)
		},
	}
}

func (f routeFixture) claim(t *testing.T, itemID string) {
	t.Helper()
	ledger := f.ledger(t)
	ok, _, err := ledger.ClaimScoped(f.key(itemID), f.tx.runID, f.tx.workflow, time.Hour)
	if err != nil || !ok {
		t.Fatalf("seed claim for %s: ok=%v err=%v", itemID, ok, err)
	}
}

func (f routeFixture) leaseHeld(t *testing.T, itemID string) bool {
	t.Helper()
	_, held := f.ledger(t).LookupScoped(f.key(itemID))
	return held
}

func TestRouteAppliesLabelsThenReleasesOwnership(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": {providers.LabelClaimed}})
	fixture := newRouteFixture(t, provider)
	fixture.claim(t, "42")

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed", "repo:dev-brandiv"}})

	if got.Outcome != routeOutcomeRouted {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Error, routeOutcomeRouted)
	}
	if len(provider.updateCalls) != 1 || len(provider.releaseCalls) != 1 {
		t.Fatalf("update calls = %v, release calls = %v; want exactly one of each", provider.updateCalls, provider.releaseCalls)
	}
	for _, want := range []string{"goobers:routed", "repo:dev-brandiv"} {
		if !containsLabel(provider.labels["42"], want) {
			t.Fatalf("label %q was not applied; labels = %v", want, provider.labels["42"])
		}
	}
	if fixture.leaseHeld(t, "42") {
		t.Fatal("the authoritative lease was not released after a successful route")
	}
}

// TestRouteRefusesWithoutOwnedLease is the guarantee same-instance selector
// validation cannot provide: without a live backlog-scoped lease held by THIS
// run, no label is applied at all.
func TestRouteRefusesWithoutOwnedLease(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": nil})
	fixture := newRouteFixture(t, provider)
	// Deliberately no claim seeded.

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeFailed {
		t.Fatalf("outcome = %q, want %q", got.Outcome, routeOutcomeFailed)
	}
	if len(provider.updateCalls) != 0 {
		t.Fatalf("labels were mutated without an owned lease: %v", provider.updateCalls)
	}
	if !strings.Contains(got.Error, "does not own a live lease") {
		t.Fatalf("error = %q, want it to name the missing lease", got.Error)
	}
}

// TestRouteRefusesWhenLeaseHeldByAnotherRun covers the shared-backlog race
// directly: a sibling run owning the item must stop this router cold.
func TestRouteRefusesWhenLeaseHeldByAnotherRun(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": nil})
	fixture := newRouteFixture(t, provider)
	ledger := fixture.ledger(t)
	if ok, _, err := ledger.ClaimScoped(fixture.key("42"), "some-other-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed foreign claim: ok=%v err=%v", ok, err)
	}

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeFailed {
		t.Fatalf("outcome = %q, want %q", got.Outcome, routeOutcomeFailed)
	}
	if len(provider.updateCalls) != 0 {
		t.Fatalf("labels were mutated over another run's lease: %v", provider.updateCalls)
	}
	if !fixture.leaseHeld(t, "42") {
		t.Fatal("the other run's lease was released")
	}
}

// TestRouteRetainsLeaseWhenLabelApplicationFails: nothing was surrendered, so
// the item stays owned and invisible to destinations.
func TestRouteRetainsLeaseWhenLabelApplicationFails(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": nil})
	provider.updateErr["42"] = errProviderBoom
	fixture := newRouteFixture(t, provider)
	fixture.claim(t, "42")

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeFailed {
		t.Fatalf("outcome = %q, want %q", got.Outcome, routeOutcomeFailed)
	}
	if len(provider.releaseCalls) != 0 {
		t.Fatal("the provider claim marker was released after a failed label application")
	}
	if !fixture.leaseHeld(t, "42") {
		t.Fatal("the authoritative lease was released after a failed label application")
	}
}

// TestRouteRetainsLeaseWhenMarkerReleaseFails is the recoverable middle state:
// labels are durable, ownership is not yet handed off, so the lease MUST be
// retained or a destination could claim an item still carrying a stale marker.
func TestRouteRetainsLeaseWhenMarkerReleaseFails(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": nil})
	provider.releaseErr["42"] = errProviderBoom
	fixture := newRouteFixture(t, provider)
	fixture.claim(t, "42")

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeFailed {
		t.Fatalf("outcome = %q, want %q", got.Outcome, routeOutcomeFailed)
	}
	if !containsLabel(provider.labels["42"], "goobers:routed") {
		t.Fatal("routing labels should already be durable at this point")
	}
	if !fixture.leaseHeld(t, "42") {
		t.Fatal("the authoritative lease must be retained for retry after a provider marker failure")
	}
	if len(got.AppliedLabels) == 0 {
		t.Fatal("the result must record which labels were applied before the failure")
	}
}

// TestRouteIsIdempotentAfterCrash reproduces §5.5's convergence case: labels
// landed and the lease is gone (a crash between the two, or a completed prior
// attempt). Re-running must report alreadyRouted, not fight for the lease.
func TestRouteIsIdempotentAfterCrash(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": {"goobers:routed", "repo:dev-brandiv"}})
	fixture := newRouteFixture(t, provider)
	// No lease: it was already released.

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed", "repo:dev-brandiv"}})

	if got.Outcome != routeOutcomeAlreadyRouted {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Error, routeOutcomeAlreadyRouted)
	}
	if len(provider.updateCalls) != 0 {
		t.Fatalf("an already-routed item was mutated again: %v", provider.updateCalls)
	}
}

// TestRouteReRunAfterMarkerFailureConverges is the retry that follows
// TestRouteRetainsLeaseWhenMarkerReleaseFails: the lease is still held and the
// labels are already present, so the retry completes the handoff without
// double-applying labels.
func TestRouteReRunAfterMarkerFailureConverges(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": {"goobers:routed"}})
	fixture := newRouteFixture(t, provider)
	fixture.claim(t, "42")

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeRouted {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Error, routeOutcomeRouted)
	}
	if len(provider.updateCalls) != 0 {
		t.Fatalf("already-present labels were re-applied: %v", provider.updateCalls)
	}
	if fixture.leaseHeld(t, "42") {
		t.Fatal("the retry did not complete the ownership handoff")
	}
}

// TestRouteBatchRecordsPerItemOutcomes proves one item's provider failure
// neither blocks the rest of the batch nor corrupts their outcomes.
func TestRouteBatchRecordsPerItemOutcomes(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{
		"1": nil,
		"2": nil,
		"3": {"goobers:routed"},
	})
	provider.updateErr["2"] = errProviderBoom
	fixture := newRouteFixture(t, provider)
	fixture.claim(t, "1")
	fixture.claim(t, "2")
	// Item 3 is already routed and unowned.

	plan := []routePlanItem{
		{ID: "1", AddLabels: []string{"goobers:routed"}},
		{ID: "2", AddLabels: []string{"goobers:routed"}},
		{ID: "3", AddLabels: []string{"goobers:routed"}},
	}
	want := map[string]string{
		"1": routeOutcomeRouted,
		"2": routeOutcomeFailed,
		"3": routeOutcomeAlreadyRouted,
	}
	for _, item := range plan {
		got := fixture.tx.routeOne(item)
		if got.Outcome != want[item.ID] {
			t.Fatalf("item %s outcome = %q (%s), want %q", item.ID, got.Outcome, got.Error, want[item.ID])
		}
	}
	if fixture.leaseHeld(t, "1") {
		t.Fatal("the successfully routed item kept its lease")
	}
	if !fixture.leaseHeld(t, "2") {
		t.Fatal("the failed item lost its lease")
	}
}

// --- Post-fetch authorization re-verification (§5.4) ---

// A claim proves ownership at claim time, not authorization at mutation time.
// Revoking the trust label is how a human withdraws consent once a router has
// already picked an item up, and it does not touch the lease — so without the
// post-fetch re-check the router would route work whose authorization was
// explicitly removed and hand it to a destination as though approved.

const routeTrustLabel = "goobers:route-approved"

// gatedRouteFixture is newRouteFixture plus the trust/selector gate a
// configured router carries.
func gatedRouteFixture(t *testing.T, provider backlogIssueProvider, gate routeLabelGate) routeFixture {
	t.Helper()
	fixture := newRouteFixture(t, provider)
	fixture.tx.gate = gate
	return fixture
}

// TestRouteRefusesWhenTrustLabelWasRevoked is the core regression: the run
// still owns the lease, the plan is valid, and the item is fetched fine — but
// its trust label is gone, so nothing may be mutated and the claim must stay.
func TestRouteRefusesWhenTrustLabelWasRevoked(t *testing.T) {
	// Claimed, but the trust label has been stripped since the claim.
	provider := newFakeRouteProvider(map[string][]string{"42": {providers.LabelClaimed}})
	fixture := gatedRouteFixture(t, provider, routeLabelGate{trustLabel: routeTrustLabel})
	fixture.claim(t, "42")

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeFailed {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Error, routeOutcomeFailed)
	}
	if len(provider.updateCalls) != 0 {
		t.Fatalf("a revoked item was labeled: %v", provider.updateCalls)
	}
	if len(provider.releaseCalls) != 0 {
		t.Fatalf("a revoked item's provider claim marker was released: %v", provider.releaseCalls)
	}
	if !fixture.leaseHeld(t, "42") {
		t.Fatal("the claim must be retained so a revoked item stays invisible to destination gaggles")
	}
	if !strings.Contains(got.Error, routeTrustLabel) {
		t.Fatalf("error = %q, want it to name the revoked trust label", got.Error)
	}
}

// TestRouteRefusesRevokedTrustLabelEvenWhenLabelsAlreadyPresent closes the
// second mutation door: an item whose routing labels already landed still has
// its provider marker released and its lease handed off, so the gate must be
// checked before that path too, not only before the label update.
func TestRouteRefusesRevokedTrustLabelEvenWhenLabelsAlreadyPresent(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": {providers.LabelClaimed, "goobers:routed"}})
	fixture := gatedRouteFixture(t, provider, routeLabelGate{trustLabel: routeTrustLabel})
	fixture.claim(t, "42")

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeFailed {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Error, routeOutcomeFailed)
	}
	if len(provider.releaseCalls) != 0 {
		t.Fatalf("ownership was handed off for a revoked item: %v", provider.releaseCalls)
	}
	if !fixture.leaseHeld(t, "42") {
		t.Fatal("the claim must be retained after a revoked-authorization refusal")
	}
}

// TestRouteRefusesWhenRequiredRoutingLabelWasRemoved covers the selector half:
// the routing labels the claim query selected on must still hold at mutation.
func TestRouteRefusesWhenRequiredRoutingLabelWasRemoved(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": {providers.LabelClaimed, routeTrustLabel}})
	fixture := gatedRouteFixture(t, provider, routeLabelGate{
		trustLabel:    routeTrustLabel,
		requireLabels: []string{"goobers:needs-routing"},
	})
	fixture.claim(t, "42")

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeFailed {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Error, routeOutcomeFailed)
	}
	if len(provider.updateCalls) != 0 || len(provider.releaseCalls) != 0 {
		t.Fatalf("a de-selected item was mutated: updates=%v releases=%v", provider.updateCalls, provider.releaseCalls)
	}
	if !fixture.leaseHeld(t, "42") {
		t.Fatal("the claim must be retained after a removed required label")
	}
	if !strings.Contains(got.Error, "goobers:needs-routing") {
		t.Fatalf("error = %q, want it to name the removed required label", got.Error)
	}
}

// TestRouteProceedsWhenTrustAndRequiredLabelsStillHold is the necessary
// converse: the gate refuses revocation, not routing.
func TestRouteProceedsWhenTrustAndRequiredLabelsStillHold(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{
		"42": {providers.LabelClaimed, routeTrustLabel, "goobers:needs-routing"},
	})
	fixture := gatedRouteFixture(t, provider, routeLabelGate{
		trustLabel:    routeTrustLabel,
		requireLabels: []string{"goobers:needs-routing"},
	})
	fixture.claim(t, "42")

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeRouted {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Error, routeOutcomeRouted)
	}
	if len(provider.updateCalls) != 1 || len(provider.releaseCalls) != 1 {
		t.Fatalf("updates=%v releases=%v; want exactly one of each", provider.updateCalls, provider.releaseCalls)
	}
	if fixture.leaseHeld(t, "42") {
		t.Fatal("a still-authorized route did not hand off ownership")
	}
}

// TestRouteGateDoesNotBlockIdempotentConvergence keeps the §5.5 retry path
// working: an already-routed, already-released item reports alreadyRouted. That
// route COMMITTED under a valid authorization; re-reporting it must not be
// re-litigated against a later revocation, and there is nothing left to mutate.
func TestRouteGateDoesNotBlockIdempotentConvergence(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": {"goobers:routed"}})
	fixture := gatedRouteFixture(t, provider, routeLabelGate{trustLabel: routeTrustLabel})
	// No lease: it was already released by the committed route.

	got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}})

	if got.Outcome != routeOutcomeAlreadyRouted {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Error, routeOutcomeAlreadyRouted)
	}
	if len(provider.updateCalls) != 0 || len(provider.releaseCalls) != 0 {
		t.Fatalf("a converged item was mutated: updates=%v releases=%v", provider.updateCalls, provider.releaseCalls)
	}
}

// TestRouteGateIsEvaluatedOnFreshlyFetchedLabels proves the check reads the
// item as the provider currently reports it, not any earlier snapshot: an item
// that gains its trust label between claim and route is routable.
func TestRouteGateIsEvaluatedOnFreshlyFetchedLabels(t *testing.T) {
	provider := newFakeRouteProvider(map[string][]string{"42": {providers.LabelClaimed}})
	fixture := gatedRouteFixture(t, provider, routeLabelGate{trustLabel: routeTrustLabel})
	fixture.claim(t, "42")

	if got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}}); got.Outcome != routeOutcomeFailed {
		t.Fatalf("outcome = %q, want %q before the label is granted", got.Outcome, routeOutcomeFailed)
	}
	provider.labels["42"] = append(provider.labels["42"], routeTrustLabel)
	if got := fixture.tx.routeOne(routePlanItem{ID: "42", AddLabels: []string{"goobers:routed"}}); got.Outcome != routeOutcomeRouted {
		t.Fatalf("outcome = %q, want %q once the label is present", got.Outcome, routeOutcomeRouted)
	}
}

// TestRouteGateEmptyAuthorizesEverything keeps a router that declares neither
// trustLabel nor requireLabels on exactly its previous behavior.
func TestRouteGateEmptyAuthorizesEverything(t *testing.T) {
	if err := (routeLabelGate{}).verify(providers.WorkItem{ID: "42"}); err != nil {
		t.Fatalf("an empty gate must authorize: %v", err)
	}
	if err := (routeLabelGate{trustLabel: "  ", requireLabels: []string{"", " "}}).verify(providers.WorkItem{ID: "42"}); err != nil {
		t.Fatalf("blank gate entries must be ignored, got: %v", err)
	}
}

// --- Allowlist and plan validation (§5.4) ---

func TestValidateRouteAllowlistRejectsPatternsMatchingReservedLabels(t *testing.T) {
	reserved := reservedRouteLabelsFor("goobers:route-approved", providers.LabelClaimed)
	for _, entry := range []string{
		"*",
		"goobers:*",
		providers.LabelApproved,
		"goobers:approv*",
		"goobers:route-approved",
		providers.LabelClaimed,
	} {
		if err := validateRouteAllowlist([]string{entry}, reserved); err == nil {
			t.Errorf("allowlist entry %q must be rejected: it can grant trust or claim", entry)
		}
	}
}

func TestValidateRouteAllowlistRejectsMalformedEntries(t *testing.T) {
	reserved := reservedRouteLabelsFor("", "")
	for _, entry := range []string{"repo:*x", "re*po:*", "repo: name"} {
		if err := validateRouteAllowlist([]string{entry}, reserved); err == nil {
			t.Errorf("allowlist entry %q must be rejected as malformed", entry)
		}
	}
	if err := validateRouteAllowlist(nil, reserved); err == nil {
		t.Error("an empty allowlist must be rejected")
	}
}

func TestValidateRouteAllowlistAcceptsReferenceTopology(t *testing.T) {
	reserved := reservedRouteLabelsFor("goobers:route-approved", providers.LabelClaimed)
	allowlist := parseRouteAllowlist("goobers:routed,repo:*,workflow:*")
	if err := validateRouteAllowlist(allowlist, reserved); err != nil {
		t.Fatalf("the reference allowlist must be accepted: %v", err)
	}
	for _, label := range []string{"goobers:routed", "repo:dev-brandiv", "workflow:implementation"} {
		if err := validateRouteLabel(label, allowlist, reserved); err != nil {
			t.Errorf("label %q must be allowed: %v", label, err)
		}
	}
	for _, label := range []string{"", providers.LabelApproved, "goobers:route-approved", "unrelated"} {
		if err := validateRouteLabel(label, allowlist, reserved); err == nil {
			t.Errorf("label %q must be rejected", label)
		}
	}
}

func TestParseRouteAllowlist(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"goobers:routed", 1},
		{"goobers:routed,repo:*,workflow:*", 3},
		{" goobers:routed , repo:* ,, ", 2},
	} {
		if got := parseRouteAllowlist(tc.in); len(got) != tc.want {
			t.Errorf("parseRouteAllowlist(%q) = %v, want %d entries", tc.in, got, tc.want)
		}
	}
}

func TestValidateRoutePlanRejectsDuplicateAndEmptyItems(t *testing.T) {
	allowlist := []string{"goobers:routed"}
	reserved := reservedRouteLabelsFor("", "")
	for name, plan := range map[string]routePlan{
		"empty id":   {Items: []routePlanItem{{ID: " ", AddLabels: []string{"goobers:routed"}}}},
		"no labels":  {Items: []routePlanItem{{ID: "1"}}},
		"duplicate":  {Items: []routePlanItem{{ID: "1", AddLabels: []string{"goobers:routed"}}, {ID: "1", AddLabels: []string{"goobers:routed"}}}},
		"bad label":  {Items: []routePlanItem{{ID: "1", AddLabels: []string{"nope"}}}},
		"reserved":   {Items: []routePlanItem{{ID: "1", AddLabels: []string{providers.LabelApproved}}}},
		"whitespace": {Items: []routePlanItem{{ID: "1", AddLabels: []string{"  "}}}},
	} {
		if err := validateRoutePlan(plan, allowlist, reserved); err == nil {
			t.Errorf("plan %q must be rejected", name)
		}
	}
}

func TestLabelsPresent(t *testing.T) {
	if !labelsPresent([]string{"a", "b"}, []string{"a"}) {
		t.Error("subset should be present")
	}
	if labelsPresent([]string{"a"}, []string{"a", "b"}) {
		t.Error("missing label should not be reported present")
	}
	if !labelsPresent(nil, nil) {
		t.Error("empty requirement is trivially present")
	}
}

// --- Mode selection ---

func TestSelectBacklogQueryModeRouteIsMutuallyExclusive(t *testing.T) {
	if mode, ok := selectBacklogQueryMode(false, false, false, false, true); !ok || mode != backlogQueryModeRoute {
		t.Fatalf("--route alone: mode=%v ok=%v", mode, ok)
	}
	for _, other := range []struct{ readOnly, claim, reconcile, release bool }{
		{readOnly: true}, {claim: true}, {reconcile: true}, {release: true},
	} {
		if _, ok := selectBacklogQueryMode(other.readOnly, other.claim, other.reconcile, other.release, true); ok {
			t.Fatalf("--route combined with %+v must be rejected", other)
		}
	}
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

var errProviderBoom = errProviderBoomType{}

type errProviderBoomType struct{}

func (errProviderBoomType) Error() string { return "provider unavailable" }
