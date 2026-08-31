package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// `goobers backlog-query --route` is the deterministic half of the validated
// handoff transaction personal-gaggle-routing §4.2 describes: an agent may
// DECIDE a route (by emitting route-plan.json), but only this code may APPLY
// one, and only under the authoritative claim the router already holds.
//
// The ordering below is load-bearing and is the whole reason routing is not
// "agent applies labels, separate stage releases":
//
//	own the backlog-scoped lease  ->  apply labels  ->  release provider marker
//	                              ->  release ledger lease
//
//   - Failing BEFORE labels changes nothing: the router keeps the lease and the
//     item is retried on the next tick.
//   - Failing AFTER labels are applied RETAINS the authoritative lease, so no
//     destination gaggle can claim a half-routed item; the retry converges via
//     the alreadyRouted path below.
//   - Releasing the ledger lease LAST means ownership transfers only once the
//     durable routing state (labels) and the provider marker already agree.
//
// A batch is per-item transactional: one item's provider failure never blocks
// the rest, every outcome is recorded, and the command exits non-zero if any
// item failed.

const (
	routePlanSchemaVersion   = "goobers.dev/backlog-route-plan/v1"
	routeResultSchemaVersion = "goobers.dev/backlog-route-result/v1"
)

// Route outcomes (§5.6).
const (
	routeOutcomeRouted        = "routed"
	routeOutcomeAlreadyRouted = "alreadyRouted"
	routeOutcomeFailed        = "failed"
)

type routePlan struct {
	SchemaVersion string          `json:"schemaVersion"`
	Items         []routePlanItem `json:"items"`
}

type routePlanItem struct {
	ID        string   `json:"id"`
	AddLabels []string `json:"addLabels"`
}

type routeResult struct {
	SchemaVersion string            `json:"schemaVersion"`
	Items         []routeResultItem `json:"items"`
}

type routeResultItem struct {
	ID            string   `json:"id"`
	Outcome       string   `json:"outcome"`
	AppliedLabels []string `json:"appliedLabels,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// --- Route label allowlist (§5.4) ---

// reservedRouteLabels is the static denylist a route transaction may never
// apply regardless of allowlist. These labels carry trust or ownership meaning
// whose lifecycle belongs to humans (approval) or to the claim transaction
// (the provider marker) — routing must never be able to grant either.
var reservedRouteLabels = []string{
	providers.LabelApproved,
	providers.LabelClaimed,
}

// routeLabelAllowed reports whether label is admitted by the static allowlist.
// Entries are exact labels or a single trailing-"*" prefix wildcard.
func routeLabelAllowed(label string, allowlist []string) bool {
	for _, entry := range allowlist {
		if prefix, wildcard := strings.CutSuffix(entry, "*"); wildcard {
			if prefix != "" && strings.HasPrefix(label, prefix) {
				return true
			}
			continue
		}
		if label == entry {
			return true
		}
	}
	return false
}

// parseRouteAllowlist splits the comma-separated allowedRouteLabels input.
func parseRouteAllowlist(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validateRouteAllowlist rejects an allowlist that could ever admit a reserved
// or trust label, BEFORE any item is examined. Validating the allowlist itself
// (rather than only the labels a plan happens to request) is what stops an
// over-broad pattern like "goobers:*" from being one adversarial plan away from
// granting approval — §5.4's "broad patterns that match reserved labels".
func validateRouteAllowlist(allowlist []string, reserved []string) error {
	if len(allowlist) == 0 {
		return errors.New("allowedRouteLabels must declare at least one entry")
	}
	for _, entry := range allowlist {
		if strings.ContainsAny(entry, ", \t\n") {
			return fmt.Errorf("allowlist entry %q must not contain whitespace or a comma", entry)
		}
		if strings.Count(entry, "*") > 1 || (strings.Contains(entry, "*") && !strings.HasSuffix(entry, "*")) {
			return fmt.Errorf("allowlist entry %q supports only a single trailing %q wildcard", entry, "*")
		}
		if entry == "*" {
			return errors.New(`allowlist entry "*" matches every label, including reserved trust labels`)
		}
		for _, blocked := range reserved {
			if blocked == "" {
				continue
			}
			if routeLabelAllowed(blocked, []string{entry}) {
				return fmt.Errorf("allowlist entry %q can match reserved label %q; routing must not be able to grant trust", entry, blocked)
			}
		}
	}
	return nil
}

// validateRouteLabel checks one requested label against the denylist and the
// allowlist. The denylist is checked first so a reserved label is refused with
// the reason that matters even if the allowlist somehow admitted it.
func validateRouteLabel(label string, allowlist []string, reserved []string) error {
	if strings.TrimSpace(label) == "" {
		return errors.New("empty routing label")
	}
	for _, blocked := range reserved {
		if blocked != "" && label == blocked {
			return fmt.Errorf("routing label %q is reserved (trust or claim label)", label)
		}
	}
	if !routeLabelAllowed(label, allowlist) {
		return fmt.Errorf("routing label %q is not in the static allowlist %v", label, allowlist)
	}
	return nil
}

// reservedRouteLabelsFor is the effective denylist for one invocation: the
// static reserved set plus the consuming workflow's own configured trust label
// and the active provider claim label (§5.4).
func reservedRouteLabelsFor(trustLabel, claimLabel string) []string {
	reserved := append([]string(nil), reservedRouteLabels...)
	for _, extra := range []string{strings.TrimSpace(trustLabel), strings.TrimSpace(claimLabel)} {
		if extra == "" {
			continue
		}
		if !slicesContains(reserved, extra) {
			reserved = append(reserved, extra)
		}
	}
	return reserved
}

func slicesContains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// --- Post-fetch authorization re-verification (§5.4) ---

// routeLabelGate is the authorization the router must still hold at the instant
// it mutates, as opposed to the instant it claimed.
//
// A claim proves only that this run owned the item when the claim was taken.
// Between that moment and this one a human can REVOKE the trust label — which
// is precisely how a human withdraws consent for automation to act on an item,
// and the only lever they have once a router has already picked the item up. A
// router that re-checked nothing would route work whose authorization had been
// explicitly withdrawn, and would then hand it to a destination gaggle as
// though it were approved. The ownership recheck cannot cover this: it asks
// "does this run still own the lease", which stays true across a revocation.
//
// So the gate is evaluated against the item as freshly FETCHED (never against
// the selection snapshot the claim was made from), immediately before the first
// mutation, and a failure is terminal for that item: no label update, no
// provider marker release, no ledger release. Retaining the claim is
// deliberate — it keeps the revoked item invisible to destination gaggles
// instead of publishing it, and leaves the ordinary lease expiry / reconcile
// path to return it to the pool.
type routeLabelGate struct {
	// trustLabel is the workflow's configured trust label: the human-granted
	// authorization for automation to act on the item at all.
	trustLabel string
	// requireLabels are the routing selector's required labels — the same
	// requireLabels the claim query selected on, re-asserted at mutation time.
	requireLabels []string
}

// verify reports why the freshly-fetched item is no longer routable, or nil.
// An empty gate authorizes everything, which keeps a router that declares
// neither input on exactly its previous behavior.
func (g routeLabelGate) verify(item providers.WorkItem) error {
	if trust := strings.TrimSpace(g.trustLabel); trust != "" && !item.HasLabel(trust) {
		return fmt.Errorf("router trust label %q is no longer present on the item; routing authorization was revoked after the claim was taken", trust)
	}
	for _, required := range g.requireLabels {
		if required = strings.TrimSpace(required); required != "" && !item.HasLabel(required) {
			return fmt.Errorf("required routing label %q is no longer present on the item; it was removed after the claim was taken", required)
		}
	}
	return nil
}

// --- Route transaction ---

// routeTransaction is one invocation's resolved, immutable context.
type routeTransaction struct {
	layout          instance.Layout
	lockPath        string
	ledgerPath      string
	runID           string
	workflow        string
	gaggle          string
	backlogIdentity apiv1.BacklogIdentity
	backlogRepo     providers.RepositoryRef
	provider        backlogIssueProvider
	gate            routeLabelGate
	instanceLog     *journal.InstanceLog
	stderr          io.Writer
}

func runBacklogQueryRoute(env backlogQueryEnv) int {
	root, l := env.root, env.layout
	stdout, stderr := env.stdout, env.stderr

	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	planFile := providerInput("routePlanFile", "route-plan.json")
	resultFile := providerInput("resultFile", "route-result.json")
	// allowedRouteLabels is deliberately read only from the task's own declared
	// inputs. §5.4 forbids sourcing it from inputsFrom, so a preceding agentic
	// stage cannot widen the very allowlist that constrains it; the compiler
	// and config validator refuse such a mapping outright
	// (internal/workflow/*/policy_actions.go staticRouteInputs), because a
	// runtime read cannot tell an inputsFrom override from a static value.
	allowlist := parseRouteAllowlist(providerInput("allowedRouteLabels", ""))
	trustLabel := providerInput("trustLabel", "")
	reserved := reservedRouteLabelsFor(trustLabel, providerInput("claimLabel", ""))
	if err := validateRouteAllowlist(allowlist, reserved); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// The same trust/selector labels the claim query selected on, re-asserted
	// against each item as freshly fetched immediately before mutation.
	gate := routeLabelGate{
		trustLabel:    trustLabel,
		requireLabels: splitLabelList(providerInput("requireLabels", "")),
	}

	plan, err := readRoutePlan(planFile)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Every label in the whole plan is validated before ANY mutation, so a plan
	// containing one forbidden label cannot partially apply.
	if err := validateRoutePlan(plan, allowlist, reserved); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if len(plan.Items) == 0 {
		if err := writeRouteResult(resultFile, routeResult{SchemaVersion: routeResultSchemaVersion}); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		pln(stdout, "route plan is empty; nothing to route")
		return 0
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Routing hands ownership between gaggles, so it is only meaningful for a
	// gaggle-owned run: without a gaggle there is no sibling to hand off to,
	// and the lease this transaction must prove it owns would have been taken
	// under the unscoped key instead.
	gaggle := providerGaggle()
	if gaggle == "" {
		pf(stderr, "error: --route requires a gaggle-owned run (GOOBERS_GAGGLE is unset)\n")
		return 1
	}
	// Routing mutates ownership, so an unresolvable backlog identity is fatal:
	// without it the lease recheck below could not prove this run owns the item
	// in THIS backlog rather than merely in this gaggle.
	identity, err := backlogIdentityForStage(root, repo)
	if err != nil {
		pf(stderr, "error: resolve backlog identity: %v\n", err)
		return 1
	}
	backlogRepo, err := backlogRepositoryRefForStage(root, repo)
	if err != nil {
		pf(stderr, "error: resolve backlog repository: %v\n", err)
		return 1
	}
	issueProvider, err := newBacklogIssueProviderForStage(root, repo, backlogRepo)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		pf(stderr, "error: open instance log: %v\n", err)
		return 1
	}
	defer func() { _ = instanceLog.Close() }()

	tx := routeTransaction{
		layout:          l,
		lockPath:        filepath.Join(l.SchedulerDir(), claimLockFileName),
		ledgerPath:      filepath.Join(l.SchedulerDir(), claimLedgerFileName),
		runID:           runID,
		workflow:        workflow,
		gaggle:          gaggle,
		backlogIdentity: identity,
		backlogRepo:     backlogRepo,
		provider:        issueProvider,
		gate:            gate,
		instanceLog:     instanceLog,
		stderr:          stderr,
	}

	results := make([]routeResultItem, 0, len(plan.Items))
	anyFailed := false
	for _, planItem := range plan.Items {
		outcome := tx.routeOne(planItem)
		results = append(results, outcome)
		switch outcome.Outcome {
		case routeOutcomeRouted:
			pf(stdout, "routed %s: %s\n", outcome.ID, strings.Join(outcome.AppliedLabels, ", "))
		case routeOutcomeAlreadyRouted:
			pf(stdout, "already routed %s\n", outcome.ID)
		default:
			anyFailed = true
			pf(stderr, "warning: failed to route %s: %s\n", outcome.ID, outcome.Error)
		}
	}

	// The result artifact is written even when items failed: a downstream gate
	// or an operator needs the per-item record precisely in the failure case.
	if err := writeRouteResult(resultFile, routeResult{SchemaVersion: routeResultSchemaVersion, Items: results}); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if anyFailed {
		return 1
	}
	return 0
}

// routeOne executes the route transaction for a single plan item. It never
// returns an error: a per-item failure is a recorded outcome so the batch
// continues (§5.5).
func (tx routeTransaction) routeOne(planItem routePlanItem) routeResultItem {
	failed := func(format string, args ...any) routeResultItem {
		return routeResultItem{ID: planItem.ID, Outcome: routeOutcomeFailed, Error: fmt.Sprintf(format, args...)}
	}

	ctx, cancel := providerCommandContext()
	defer cancel()

	// Step 1 — prove ownership. The lease is rechecked under the claim lock
	// immediately before mutating, against the BACKLOG-scoped key: a
	// same-instance selector check, or a gaggle-scoped lookup, would both
	// happily confirm "someone in my gaggle owns it" while a sibling gaggle
	// sharing this backlog owned it too.
	owned, err := tx.ownsLease(planItem.ID)
	if err != nil {
		return failed("check backlog lease: %v", err)
	}

	item, err := tx.provider.GetWorkItem(ctx, tx.backlogRepo, planItem.ID)
	if err != nil {
		return failed("fetch item: %v", err)
	}

	if !owned {
		// Idempotent convergence (§5.5): a crash after labels were applied but
		// before the lease was released leaves the labels present and the lease
		// gone. Re-running must report success, not fight for a lease that has
		// legitimately moved on.
		if labelsPresent(item.Labels, planItem.AddLabels) {
			return routeResultItem{
				ID:            planItem.ID,
				Outcome:       routeOutcomeAlreadyRouted,
				AppliedLabels: planItem.AddLabels,
			}
		}
		return failed("this run does not own a live lease for %s in backlog %s; refusing to apply routing labels",
			planItem.ID, tx.backlogIdentity.String())
	}

	// Step 1b — prove the item is still AUTHORIZED, against the item as just
	// fetched rather than the snapshot the claim was made from. Ownership and
	// authorization are independent: a human revoking the trust label withdraws
	// consent without touching the lease, so ownsLease above stays true. This
	// must precede BOTH mutations — the label update and the marker release —
	// because either one publishes routing state for an item whose
	// authorization is gone. Failing here retains the claim on purpose: the
	// revoked item stays invisible to destination gaggles rather than being
	// handed to one.
	if err := tx.gate.verify(item); err != nil {
		return failed("%v; refusing to route and retaining the claim", err)
	}

	// Already-correct item that we still own: apply nothing, but continue to
	// release so ownership is handed off exactly once.
	if !labelsPresent(item.Labels, planItem.AddLabels) {
		// Step 2 — apply routing labels. This is the durable routing state; it
		// must land before any ownership is surrendered.
		if _, err := tx.provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: tx.backlogRepo,
			ID:         planItem.ID,
			AddLabels:  planItem.AddLabels,
		}); err != nil {
			// Nothing was surrendered, so the retained lease keeps the item
			// invisible to destinations and the next tick retries cleanly.
			return failed("apply routing labels: %v", err)
		}
	}

	// Step 3 — end the provider claim epoch. On failure the authoritative
	// ledger lease is deliberately RETAINED: labels are applied but ownership
	// has not transferred, which is the recoverable state, whereas releasing
	// the ledger here would publish an item still carrying a stale marker.
	if _, err := tx.provider.ReleaseWorkItemClaim(ctx, providers.ClaimWorkItemRequest{
		Repository:       tx.backlogRepo,
		ID:               planItem.ID,
		RunID:            tx.runID,
		LedgerAuthorized: true,
	}); err != nil {
		return routeResultItem{
			ID:            planItem.ID,
			Outcome:       routeOutcomeFailed,
			AppliedLabels: planItem.AddLabels,
			Error:         fmt.Sprintf("release provider claim marker (routing labels applied; local lease retained for retry): %v", err),
		}
	}

	// Step 4 — release the authoritative lease. Only now may a destination
	// gaggle claim this item.
	if err := tx.releaseLease(planItem.ID); err != nil {
		return routeResultItem{
			ID:            planItem.ID,
			Outcome:       routeOutcomeFailed,
			AppliedLabels: planItem.AddLabels,
			Error:         fmt.Sprintf("release ledger lease: %v", err),
		}
	}

	tx.journalRouted(planItem, routeOutcomeRouted)
	return routeResultItem{
		ID:            planItem.ID,
		Outcome:       routeOutcomeRouted,
		AppliedLabels: planItem.AddLabels,
	}
}

// ownsLease reports whether this run holds a LIVE backlog-scoped lease for
// itemID. It reads under the claim lock so the answer cannot be invalidated by
// a concurrent claim/recovery pass between the check and the mutation that
// follows it.
func (tx routeTransaction) ownsLease(itemID string) (bool, error) {
	var owned bool
	err := withClaimLock(tx.lockPath, claimLockOperationBacklogRouteLookup, func() error {
		ledger, err := localscheduler.OpenClaimLedger(tx.ledgerPath)
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		entry, held := ledger.LookupScoped(backlogClaimKey(tx.backlogIdentity, tx.gaggle, itemID))
		owned = held && entry.RunID == tx.runID && entry.ExpiresAt.After(time.Now())
		return nil
	})
	return owned, err
}

func (tx routeTransaction) releaseLease(itemID string) error {
	return withClaimLock(tx.lockPath, claimLockOperationBacklogRoute, func() error {
		ledger, err := localscheduler.OpenClaimLedger(tx.ledgerPath, localscheduler.WithInstanceLog(tx.instanceLog))
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		return ledger.ReleaseScoped(backlogClaimKey(tx.backlogIdentity, tx.gaggle, itemID), tx.runID)
	})
}

// journalRouted emits backlog.item.routed (§5.10). Journaling is best-effort
// observability, exactly like the claim ledger's own transition events: the
// routing transaction has already committed by this point, so a journal write
// failure must not turn a completed route into a reported failure.
func (tx routeTransaction) journalRouted(planItem routePlanItem, outcome string) {
	if tx.instanceLog == nil {
		return
	}
	_ = tx.instanceLog.Append(journal.Event{
		Type:     journal.EventBacklogItemRouted,
		RunID:    tx.runID,
		Workflow: tx.workflow,
		Runner: map[string]any{
			"claimBacklog":    tx.backlogIdentity.String(),
			"claimExternalId": planItem.ID,
			"claimProvider":   string(tx.backlogIdentity.Provider),
			"gaggle":          tx.gaggle,
			"routeLabels":     planItem.AddLabels,
			"outcome":         outcome,
		},
	})
}

// --- Plan/result IO ---

func readRoutePlan(path string) (routePlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return routePlan{}, fmt.Errorf("read route plan %s: %w", path, err)
	}
	var plan routePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return routePlan{}, fmt.Errorf("parse route plan %s: %w", path, err)
	}
	if plan.SchemaVersion != routePlanSchemaVersion {
		return routePlan{}, fmt.Errorf("unsupported route plan schema %q, expected %q", plan.SchemaVersion, routePlanSchemaVersion)
	}
	return plan, nil
}

func validateRoutePlan(plan routePlan, allowlist, reserved []string) error {
	seen := make(map[string]struct{}, len(plan.Items))
	for _, item := range plan.Items {
		if strings.TrimSpace(item.ID) == "" {
			return errors.New("route plan item has an empty id")
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return fmt.Errorf("route plan lists item %s more than once", item.ID)
		}
		seen[item.ID] = struct{}{}
		if len(item.AddLabels) == 0 {
			return fmt.Errorf("route plan item %s requests no labels", item.ID)
		}
		for _, label := range item.AddLabels {
			if err := validateRouteLabel(label, allowlist, reserved); err != nil {
				return fmt.Errorf("route plan item %s: %w", item.ID, err)
			}
		}
	}
	return nil
}

func writeRouteResult(path string, result routeResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal route result: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write route result %s: %w", path, err)
	}
	return nil
}

// labelsPresent reports whether every requested label is already on the item.
func labelsPresent(present, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(present))
	for _, label := range present {
		have[label] = struct{}{}
	}
	for _, label := range required {
		if _, ok := have[label]; !ok {
			return false
		}
	}
	return true
}
